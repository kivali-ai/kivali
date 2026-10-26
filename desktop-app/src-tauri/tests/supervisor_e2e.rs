//! The shell's supervisor client against a real process on the
//! platform's real transport (a Unix socket; on Windows the named pipe):
//! the fake supervisor (always; `make test` builds it first), and the
//! real `kivali-supervisor` when KIVALI_REAL_SUPERVISOR names its binary
//! (status, check and `down --exit` only: no VM is booted).

use kivali_desktop_lib::orgurl;
use kivali_desktop_lib::supervisor::sidecar::{Sidecar, Stopped, STOP_TIMEOUTS};
use kivali_desktop_lib::supervisor::wire::{check_status, stage, BakedRelease, OrgState, SetupRequest, StageProgress};
use kivali_desktop_lib::supervisor::{UpRequest, Error, Supervisor};
use std::path::{Path, PathBuf};

fn fake_binary() -> PathBuf {
    let p = Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .unwrap()
        .join("fake-supervisor")
        .join("target")
        .join("debug")
        .join(format!("kivali-supervisor{}", std::env::consts::EXE_SUFFIX));
    assert!(
        p.exists(),
        "build the fake first: cargo build --manifest-path fake-supervisor/Cargo.toml ({} missing)",
        p.display()
    );
    p
}

fn start(binary: &Path, dir: &Path, vm_dir: Option<&Path>) -> (Supervisor, Sidecar) {
    let sup = Supervisor::new(dir);
    let probe = Supervisor::new(dir);
    let sc = Sidecar::start(binary, dir, vm_dir, &|| probe.is_listening(), &std::thread::yield_now, &std::time::Instant::now).expect("serve starts");
    (sup, sc)
}

#[test]
fn create_check_credential_upgrade_destroy_against_the_fake() {
    std::env::set_var("FAKE_STEP_MS", "0");
    std::env::set_var("FAKE_PORT", "18931");
    std::env::set_var("FAKE_BAKED", "0.18.0");
    let dir = tempfile::tempdir().unwrap();
    let (sup, sc) = start(&fake_binary(), dir.path(), None);

    assert_eq!(sup.status().unwrap().state, OrgState::Absent);

    // Creating needs an owner.
    let e = sup.up(&UpRequest::default(), &mut |_| {}).unwrap_err();
    assert!(matches!(e, Error::Supervisor(ref m) if m.contains("owner")), "{e:?}");

    let mut stages = Vec::new();
    let req = UpRequest { owner: Some("owner@example.com".into()), memory_mb: 4096, cpus: 4, ..Default::default() };
    let st = sup.up(&req, &mut |p| stages.extend(p.stage)).unwrap();
    assert_eq!((st.state, st.port, st.kivali.as_deref()), (OrgState::Running, Some(18931), Some("0.16.0")));
    stages.dedup();
    assert_eq!(stages, ["making-room", "starting", "setting-up"]);

    // The team's port answers GET /api/v1/login as a Kivali team.
    let origin = orgurl::normalize_org_url("http://127.0.0.1:18931").unwrap();
    let info = tauri::async_runtime::block_on(orgurl::fetch_login(&origin)).unwrap();
    assert_eq!(info.org_name, "Fake Org");

    // Claude: not signed in yet (the fake signs in only with FAKE_SIGNED_IN).
    let c = sup.credential().unwrap();
    assert_eq!((c.signed_in, c.email, c.billing), (false, None, None));

    // A sign-in setup (the fake's test data is Microsoft Foundry's): a
    // URL is refused in the supervisor's words; a bare name signs in and
    // every model is checked; Sign in again clears it.
    let info = sup.setup_info().unwrap();
    assert_eq!((info.models.len(), info.current), (4, None));
    let setup = |resource: &str| SetupRequest {
        setup: "microsoft-foundry".into(),
        values: [("resource", resource), ("auth", "api_key"), ("api_key", "k")].map(|(k, v)| (k.to_string(), v.to_string())).into(),
    };
    assert!(sup.apply_setup(&setup("https://r.services.ai.azure.com")).unwrap_err().sentence().starts_with("enter the resource only"));
    let r = sup.apply_setup(&setup("my-resource")).unwrap();
    assert_eq!(r.credential.billing.as_deref(), Some("Microsoft Foundry · my-resource"));
    assert_eq!(r.models.iter().map(|m| m.model.as_str()).collect::<Vec<_>>(), info.models);
    let saved = sup.setup_info().unwrap().current.unwrap();
    assert_eq!((saved.values.get("auth").map(String::as_str), saved.values.get("api_key")), (Some("api_key"), None));
    assert_eq!(sup.clear_provider().unwrap().cleared.len(), 2);
    assert!(sup.clear_provider().unwrap().cleared.is_empty());
    assert!(!sup.credential().unwrap().signed_in);

    let report = sup.check().unwrap();
    assert_eq!(report.status.as_deref(), Some(check_status::UPGRADE));
    assert_eq!(report.latest.as_deref(), Some("0.17.0"));
    assert_eq!(report.min_desktop.as_deref(), Some("0.16.0"));

    // The VM image bakes a newer Kivali than the team runs (a newer
    // desktop's), so the report says the team can move to it.
    assert_eq!(sup.report().unwrap().baked, Some(BakedRelease { version: "0.18.0".into(), newer: true, manual_steps: false }));

    let st = sup.upgrade(false, &mut |_| {}).unwrap();
    assert_eq!(st.kivali.as_deref(), Some("0.17.0"));

    // The baked update: no download, its first stage says so, and the
    // report then has nothing newer.
    let mut seen = Vec::new();
    let st = sup.upgrade(true, &mut |p| seen.push(p)).unwrap();
    assert_eq!(st.kivali.as_deref(), Some("0.18.0"));
    assert!(seen.iter().any(|p| p.label.contains("baked into the VM image")), "{seen:?}");
    assert!(!seen.iter().any(|p| p.stage.as_deref() == Some(stage::DOWNLOADING) && p.label.is_empty()), "a baked update reported download progress: {seen:?}");
    assert_eq!(sup.report().unwrap().baked, Some(BakedRelease { version: "0.18.0".into(), newer: false, manual_steps: false }));
    let e = sup.upgrade(true, &mut |_| {}).unwrap_err();
    assert!(matches!(e, Error::Supervisor(ref m) if m.contains("up to date")), "{e:?}");

    // A team whose data disk an earlier version kept in the config
    // directory: the next resume moves it first, reporting the copy's
    // and then the compaction's progress, each in its stage, and the
    // disk left behind is gone once the team is up.
    sup.down(false, &mut |_| {}).unwrap();
    let legacy = dir.path().join("data.vhdx");
    std::fs::write(&legacy, b"a data disk").unwrap();
    let mut seen = Vec::new();
    sup.up(&UpRequest::default(), &mut |p| seen.push(p)).unwrap();
    let mut stages: Vec<_> = seen.iter().filter_map(|p| p.stage.clone()).collect();
    stages.dedup();
    assert_eq!(stages, ["updating-data", "making-room", "starting", "setting-up"]);
    let reports: Vec<_> = seen.iter().filter(|p| p.label.is_empty()).map(|p| (p.stage.clone().unwrap(), p.progress.unwrap())).collect();
    let copy: Vec<_> = reports.iter().filter(|(s, _)| s == stage::UPDATING_DATA).map(|(_, p)| *p).collect();
    assert_eq!((copy.first(), copy.last()), (Some(&StageProgress { done: 0, total: 8 << 30 }), Some(&StageProgress { done: 8 << 30, total: 8 << 30 })));
    assert!(copy.windows(2).all(|w| w[0].done <= w[1].done), "{copy:?}");
    assert!(reports.iter().any(|(s, p)| s == stage::MAKING_ROOM && p.total == 100), "{reports:?}");
    assert!(!legacy.exists() && !dir.path().join("data.vhdx.migrated").exists());

    // Delete: destroy with exit ends serve, so the stop finds it gone.
    sup.destroy(true, &mut |_| {}).unwrap();
    let how = sc.stop(&mut || Ok(()), STOP_TIMEOUTS);
    assert_eq!(how, Stopped::Down);
    assert!(!sup.is_listening());

    quit_stops_the_fake_rpc_first();
}

/// Quit: one test with the create test, since both set the fake's port
/// through the (process-wide) environment.
fn quit_stops_the_fake_rpc_first() {
    let dir = tempfile::tempdir().unwrap();
    let (sup, sc) = start(&fake_binary(), dir.path(), None);
    let req = UpRequest { owner: Some("owner@example.com".into()), ..Default::default() };
    sup.up(&req, &mut |_| {}).unwrap();
    // Quit: the RPC-first stop; down --exit stops the VM and ends serve,
    // so neither the stop request nor a kill is needed.
    let how = sc.stop(&mut || sup.down(true, &mut |_| {}).map_err(|e| e.to_string()), STOP_TIMEOUTS);
    assert_eq!(how, Stopped::Down);
    assert!(!sup.is_listening());
    assert_eq!(sup.status().unwrap_err(), Error::NotRunning);
}

/// A serve whose shell is gone (here: started through a shell that exits
/// at once, so launchd inherits it) is adopted once the pid file names
/// it, and `down --exit` plus the kqueue wait end it.
#[cfg(target_os = "macos")]
#[test]
fn adopts_an_orphaned_serve() {
    use kivali_desktop_lib::platform::peer_pid;
    use kivali_desktop_lib::supervisor::sidecar::pid_path;
    let dir = tempfile::tempdir().unwrap();
    let script = format!(
        "'{}' --config-dir '{}' serve >/dev/null 2>&1 &",
        fake_binary().display(),
        dir.path().display()
    );
    assert!(std::process::Command::new("/bin/sh").arg("-c").arg(script).status().unwrap().success());
    let sup = Supervisor::new(dir.path());
    while !sup.is_listening() {
        std::thread::yield_now();
    }
    // Not recorded as ours: not adopted.
    assert!(Sidecar::adopt(dir.path(), sup.endpoint()).is_none());
    let peer = peer_pid(sup.endpoint()).unwrap();
    std::fs::write(pid_path(dir.path()), format!("{peer}\n")).unwrap();
    let sc = Sidecar::adopt(dir.path(), sup.endpoint()).expect("adopted");
    assert_eq!(sc.pid(), peer);
    sup.down(true, &mut |_| {}).unwrap();
    // Returns once the exit has begun (kqueue NOTE_EXIT), which can be a
    // moment before the socket closes, so the socket is not asserted
    // closed here; that wait returns at all, and the pid file, are.
    sc.wait();
    assert!(!pid_path(dir.path()).exists());
}

#[test]
fn real_supervisor_protocol_without_a_vm() {
    let Some(bin) = std::env::var_os("KIVALI_REAL_SUPERVISOR").map(PathBuf::from) else {
        eprintln!("KIVALI_REAL_SUPERVISOR unset; skipping the real-supervisor check");
        return;
    };
    // serve insists on a VM image directory even when it boots nothing.
    // On Windows it only looks for root.vhdx there, so without
    // KIVALI_VM_DIR an empty file of that name stands in.
    let placeholder = tempfile::tempdir().unwrap();
    let vm = match std::env::var_os("KIVALI_VM_DIR") {
        Some(d) => PathBuf::from(d),
        None if cfg!(windows) => {
            std::fs::write(placeholder.path().join("root.vhdx"), b"").unwrap();
            placeholder.path().to_path_buf()
        }
        None => Path::new(env!("CARGO_MANIFEST_DIR")).join("../../vm/build/out"),
    };
    let dir = tempfile::tempdir().unwrap();
    let (sup, sc) = start(&bin, dir.path(), Some(&vm));

    let report = sup.report().unwrap();
    assert!(!report.running, "{report:?}");
    let st = sup.status().unwrap();
    assert_eq!(st.state, OrgState::Absent, "{report:?}");

    // A feed that does not exist: the check itself succeeds and reports
    // the failure, as DESKTOP.md requires.
    let c = sup.check().unwrap();
    assert_eq!(c.status.as_deref(), Some(check_status::FAILED), "{c:?}");
    assert!(c.error.is_some());

    sup.down(true, &mut |_| {}).unwrap();
    sc.wait();
    assert!(!sup.is_listening());
}
