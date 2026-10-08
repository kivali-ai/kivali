//! The Windows transport, served as the real supervisor serves it
//! (`internal/supervisor/host/host_windows.go`, `Listen`): the named
//! pipe `\\.\pipe\kivali-<hash>` of the config directory, a DACL that
//! admits only the user running serve, and one pipe instance per
//! connection.
//!
//! The name is the shell's rule as well (`platform::pipe_name`,
//! src-tauri/src/platform/mod.rs). This crate stands alone, so the few
//! lines are repeated here and tested against the same vectors.

use std::path::Path;

/// The named pipe the supervisor serves for config directory `dir`:
/// `\\.\pipe\kivali-` and the first 8 lower-case hex characters of the
/// SHA-256 of `strings.ToLower(filepath.Clean(abs))`, `abs` being the
/// directory made absolute as both other sides make it
/// (`GetFullPathNameW`, which also resolves `.` and `..` and turns `/`
/// into `\`). Shared vector: `C:\Users\Maya\AppData\Local\Kivali` is
/// `\\.\pipe\kivali-96e3ef74`.
pub fn pipe_name(dir: &Path) -> String {
    let abs = std::path::absolute(dir).unwrap_or_else(|_| dir.to_path_buf());
    pipe_name_of_absolute(&abs.to_string_lossy())
}

/// [`pipe_name`] for a path already absolute; pure, so the rule is
/// tested on every OS with Windows spellings.
fn pipe_name_of_absolute(abs: &str) -> String {
    use sha2::{Digest, Sha256};
    let digest = Sha256::digest(pipe_key(abs).as_bytes());
    let hex: String = digest.iter().map(|b| format!("{b:02x}")).collect();
    format!(r"\\.\pipe\kivali-{}", &hex[..8])
}

/// `strings.ToLower(filepath.Clean(abs))` for an absolute path as
/// `GetFullPathNameW` gives it: runs of separators collapse to one (a UNC
/// path keeps its leading two), a trailing separator goes unless it ends
/// the root (`C:\`, `/`), and each character is lower-cased on its own,
/// as Go's `unicode.ToLower` does (so `Σ` is always `σ`, and `İ` is `i`).
fn pipe_key(abs: &str) -> String {
    let is_sep = |c: char| c == '/' || c == '\\';
    let mut out = String::with_capacity(abs.len());
    let mut prev_sep = false;
    for (i, c) in abs.chars().enumerate() {
        let sep = is_sep(c);
        if !(sep && prev_sep && i > 1) {
            out.push(c);
        }
        prev_sep = sep;
    }
    while out.chars().count() > 1 && out.ends_with(is_sep) && !out[..out.len() - 1].ends_with(':') {
        out.pop();
    }
    out.chars()
        .flat_map(|c| if c == '\u{130}' { 'i'.to_lowercase() } else { c.to_lowercase() })
        .collect()
}

#[cfg(windows)]
pub use server::Listener;

#[cfg(windows)]
mod server {
    use std::ffi::OsStr;
    use std::io;
    use std::os::windows::ffi::OsStrExt;
    use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
    use windows_sys::Win32::Foundation::{
        GetLastError, LocalFree, ERROR_NO_DATA, ERROR_PIPE_CONNECTED, HANDLE, INVALID_HANDLE_VALUE,
    };
    use windows_sys::Win32::Security::Authorization::{
        ConvertSidToStringSidW, ConvertStringSecurityDescriptorToSecurityDescriptorW, SDDL_REVISION_1,
    };
    use windows_sys::Win32::Security::{
        GetTokenInformation, TokenUser, PSECURITY_DESCRIPTOR, SECURITY_ATTRIBUTES, TOKEN_QUERY, TOKEN_USER,
    };
    use windows_sys::Win32::Storage::FileSystem::{FILE_FLAG_FIRST_PIPE_INSTANCE, PIPE_ACCESS_DUPLEX};
    use windows_sys::Win32::System::Pipes::{
        ConnectNamedPipe, CreateNamedPipeW, DisconnectNamedPipe, PIPE_READMODE_BYTE, PIPE_REJECT_REMOTE_CLIENTS,
        PIPE_TYPE_BYTE, PIPE_UNLIMITED_INSTANCES, PIPE_WAIT,
    };
    use windows_sys::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};

    /// Each direction's buffer, a size the system treats as advisory.
    const BUFFER: u32 = 64 * 1024;

    fn wide(s: &str) -> Vec<u16> {
        OsStr::new(s).encode_wide().chain(std::iter::once(0)).collect()
    }

    /// A NUL-terminated wide string the system allocated, as a String;
    /// frees it.
    ///
    /// SAFETY: `p` is a non-null, NUL-terminated string from LocalAlloc.
    unsafe fn take_local_wide(p: *mut u16) -> String {
        let len = (0..).take_while(|&i| *p.add(i) != 0).count();
        let s = String::from_utf16_lossy(std::slice::from_raw_parts(p, len));
        LocalFree(p.cast());
        s
    }

    /// This process's user SID (`S-1-5-21-…`), from its token.
    pub(super) fn user_sid() -> io::Result<String> {
        // SAFETY: a token handle we own (closed on drop), queried for the
        // size and then into a buffer aligned for TOKEN_USER, whose SID
        // points into that buffer while it lives.
        unsafe {
            let mut token: HANDLE = std::ptr::null_mut();
            if OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) == 0 {
                return Err(io::Error::last_os_error());
            }
            let token = OwnedHandle::from_raw_handle(token);
            let mut need = 0u32;
            GetTokenInformation(token.as_raw_handle(), TokenUser, std::ptr::null_mut(), 0, &mut need);
            if need == 0 {
                return Err(io::Error::last_os_error());
            }
            let mut buf = vec![0u64; (need as usize).div_ceil(8)];
            if GetTokenInformation(token.as_raw_handle(), TokenUser, buf.as_mut_ptr().cast(), need, &mut need) == 0 {
                return Err(io::Error::last_os_error());
            }
            let sid = (*(buf.as_ptr() as *const TOKEN_USER)).User.Sid;
            let mut text: *mut u16 = std::ptr::null_mut();
            if ConvertSidToStringSidW(sid, &mut text) == 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(take_local_wide(text))
        }
    }

    /// The pipe's DACL: protected (nothing inherited), one ACE granting
    /// this user generic all, as `ownerOnly` in host_windows.go. No other
    /// account is granted anything; an administrator can still take
    /// ownership and loosen it.
    pub(super) fn owner_only_sddl() -> io::Result<String> {
        Ok(format!("D:P(A;;GA;;;{})", user_sid()?))
    }

    /// A security descriptor made from SDDL; freed on drop.
    pub(super) struct Descriptor(pub(super) PSECURITY_DESCRIPTOR);

    // SAFETY: plain memory this value alone owns, only read (by
    // CreateNamedPipeW), so it may move to another thread with its owner.
    unsafe impl Send for Descriptor {}

    impl Descriptor {
        pub(super) fn from_sddl(sddl: &str) -> io::Result<Descriptor> {
            let text = wide(sddl);
            let mut sd: PSECURITY_DESCRIPTOR = std::ptr::null_mut();
            // SAFETY: a NUL-terminated string; the descriptor is
            // LocalAlloc'd for us and freed on drop.
            let ok = unsafe {
                ConvertStringSecurityDescriptorToSecurityDescriptorW(text.as_ptr(), SDDL_REVISION_1, &mut sd, std::ptr::null_mut())
            };
            if ok == 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(Descriptor(sd))
        }
    }

    impl Drop for Descriptor {
        fn drop(&mut self) {
            // SAFETY: the LocalAlloc'd descriptor, freed once.
            unsafe { LocalFree(self.0) };
        }
    }

    /// One instance of the pipe, in byte mode, refusing remote clients,
    /// with the owner-only descriptor. `first` asks that the pipe not
    /// exist yet (`FILE_FLAG_FIRST_PIPE_INSTANCE`): if it does, whoever
    /// holds it, the call fails with `ERROR_ACCESS_DENIED`.
    fn instance(name: &[u16], sd: &Descriptor, first: bool) -> io::Result<OwnedHandle> {
        let sa = SECURITY_ATTRIBUTES {
            nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: sd.0,
            bInheritHandle: 0,
        };
        let open = PIPE_ACCESS_DUPLEX | if first { FILE_FLAG_FIRST_PIPE_INSTANCE } else { 0 };
        let mode = PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS;
        // SAFETY: a NUL-terminated name and a descriptor that outlives the
        // call; the handle is owned from here on.
        unsafe {
            let h = CreateNamedPipeW(name.as_ptr(), open, mode, PIPE_UNLIMITED_INSTANCES, BUFFER, BUFFER, 0, &sa);
            if h == INVALID_HANDLE_VALUE {
                return Err(io::Error::last_os_error());
            }
            Ok(OwnedHandle::from_raw_handle(h))
        }
    }

    /// The pipe being served: always one instance waiting for the next
    /// client. Every connection takes its instance for good and a new
    /// one is made at once, so a client never finds the pipe missing
    /// while serve runs; between a connect and that new instance it is
    /// busy for a moment, which the shell waits out (`WaitNamedPipeW`).
    pub struct Listener {
        name: Vec<u16>,
        sd: Descriptor,
        waiting: OwnedHandle,
    }

    impl Listener {
        /// Creates the pipe (its first instance). A pipe that exists
        /// already, this user's or another account's, is refused with
        /// `ERROR_ACCESS_DENIED`.
        pub fn bind(name: &str) -> io::Result<Listener> {
            let sd = Descriptor::from_sddl(&owner_only_sddl()?)?;
            let name = wide(name);
            let waiting = instance(&name, &sd, true)?;
            Ok(Listener { name, sd, waiting })
        }

        /// The waiting instance, once a client has opened it, as a file
        /// for one request and its answer; a new instance waits in its
        /// place. A client that has gone reads as the end
        /// (`ERROR_BROKEN_PIPE`, which std reads as 0).
        ///
        /// Dropping the file ends the connection, as go-winio's close does
        /// for the real serve: the client still reads what is left in the
        /// pipe, then the end. Not `DisconnectNamedPipe`, which throws the
        /// unread bytes away and fails the client's next read with
        /// `ERROR_PIPE_NOT_CONNECTED`, an error to std rather than the end
        /// a close-delimited body needs. A process exit closes the same
        /// way, so the last line of a `--exit` operation arrives.
        pub fn accept(&mut self) -> io::Result<std::fs::File> {
            let h = self.waiting.as_raw_handle();
            loop {
                // SAFETY: a pipe instance we own, opened for synchronous
                // I/O, so the call blocks until a client opens it.
                if unsafe { ConnectNamedPipe(h, std::ptr::null_mut()) } != 0 {
                    break;
                }
                // SAFETY: plain error queries and a disconnect of our own
                // instance.
                match unsafe { GetLastError() } {
                    // Opened between CreateNamedPipeW and the call.
                    ERROR_PIPE_CONNECTED => break,
                    // Opened and closed again before it: wait again.
                    ERROR_NO_DATA => unsafe {
                        DisconnectNamedPipe(h);
                    },
                    e => return Err(io::Error::from_raw_os_error(e as i32)),
                }
            }
            let next = instance(&self.name, &self.sd, false)?;
            Ok(std::fs::File::from(std::mem::replace(&mut self.waiting, next)))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pipe_names_follow_the_shared_rule() {
        // The vector the supervisor's and the shell's tests share:
        // sha256(r"c:\users\maya\appdata\local\kivali") = 96e3ef747ec8bfd4…
        let v = r"\\.\pipe\kivali-96e3ef74";
        assert_eq!(pipe_name_of_absolute(r"C:\Users\Maya\AppData\Local\Kivali"), v);
        // A trailing or doubled separator, or another case, names the
        // same directory; another directory does not.
        assert_eq!(pipe_name_of_absolute(r"C:\Users\Maya\AppData\Local\Kivali\"), v);
        assert_eq!(pipe_name_of_absolute(r"c:\USERS\maya\AppData\\Local\KIVALI"), v);
        assert_ne!(pipe_name_of_absolute(r"C:\Users\Maya\AppData\Local\Kivali-dev"), v);
        if cfg!(windows) {
            // Made absolute as the shell and the supervisor make it.
            assert_eq!(pipe_name(Path::new(r"C:\Users\Maya\AppData\Local\Kivali")), v);
            assert_eq!(pipe_name(Path::new("c:/users/maya/appdata/local/kivali/")), v);
            assert_eq!(pipe_name(Path::new(r"C:\Users\Maya\AppData\Local\Kivali\.")), v);
        } else {
            // sha256("/users/me/library/application support/kivali")
            //   = 568f6dfd…, as in the shell's test.
            let a = pipe_name(Path::new("/Users/me/Library/Application Support/Kivali/"));
            assert_eq!(a, r"\\.\pipe\kivali-568f6dfd");
        }
    }

    /// The Windows spellings, as `GetFullPathNameW` hands them over; the
    /// shell's `pipe_keys_clean_and_fold_like_go` vectors.
    #[test]
    fn pipe_keys_clean_and_fold_like_go() {
        assert_eq!(pipe_key(r"C:\Users\Me\AppData\Local\Kivali"), r"c:\users\me\appdata\local\kivali");
        assert_eq!(pipe_key(r"C:\Users\Me\AppData\Local\Kivali\"), r"c:\users\me\appdata\local\kivali");
        assert_eq!(pipe_key(r"C:\Users\\Me\\\Kivali"), r"c:\users\me\kivali");
        assert_eq!(pipe_key(r"C:\"), r"c:\");
        assert_eq!(pipe_key("/"), "/");
        assert_eq!(pipe_key(r"\\Server\Share\Kivali"), r"\\server\share\kivali");
        assert_eq!(pipe_key(r"C:\ΟΔΟΣ\İ"), r"c:\οδοσ\i");
        // sha256(r"c:\users\me\appdata\local\kivali") = daac4236…
        assert_eq!(pipe_name_of_absolute(r"C:\Users\Me\AppData\Local\Kivali\"), r"\\.\pipe\kivali-daac4236");
    }

    /// The server against clients of this process, so the DACL admits
    /// them; each test has a pipe name of its own.
    #[cfg(windows)]
    mod server {
        use super::super::server::{owner_only_sddl, user_sid, Descriptor};
        use super::super::Listener;
        use std::io::{Read, Write};
        use std::os::windows::io::AsRawHandle;
        use std::sync::atomic::{AtomicUsize, Ordering};
        use std::time::Duration;
        use windows_sys::Win32::Foundation::{LocalFree, ERROR_ACCESS_DENIED, ERROR_FILE_NOT_FOUND, ERROR_PIPE_BUSY};
        use windows_sys::Win32::Security::Authorization::{
            ConvertSecurityDescriptorToStringSecurityDescriptorW, GetSecurityInfo, SDDL_REVISION_1, SE_KERNEL_OBJECT,
        };
        use windows_sys::Win32::Security::{DACL_SECURITY_INFORMATION, PSECURITY_DESCRIPTOR};

        fn unique_name() -> String {
            static N: AtomicUsize = AtomicUsize::new(0);
            format!(r"\\.\pipe\kivali-fake-test-{}-{}", std::process::id(), N.fetch_add(1, Ordering::SeqCst))
        }

        /// Opens the pipe as the shell does (read and write), waiting out a
        /// moment when every instance is busy.
        fn open(name: &str) -> std::io::Result<std::fs::File> {
            for _ in 0..500 {
                match std::fs::OpenOptions::new().read(true).write(true).open(name) {
                    Err(e) if e.raw_os_error() == Some(ERROR_PIPE_BUSY as i32) => {
                        std::thread::sleep(Duration::from_millis(10))
                    }
                    r => return r,
                }
            }
            panic!("{name} stayed busy");
        }

        /// The DACL of `sd` as SDDL.
        ///
        /// SAFETY: `sd` is a valid security descriptor.
        unsafe fn dacl_sddl(sd: PSECURITY_DESCRIPTOR) -> String {
            let mut text: *mut u16 = std::ptr::null_mut();
            let ok = ConvertSecurityDescriptorToStringSecurityDescriptorW(
                sd,
                SDDL_REVISION_1,
                DACL_SECURITY_INFORMATION,
                &mut text,
                std::ptr::null_mut(),
            );
            assert_ne!(ok, 0, "{}", std::io::Error::last_os_error());
            let len = (0..).take_while(|&i| *text.add(i) != 0).count();
            let s = String::from_utf16_lossy(std::slice::from_raw_parts(text, len));
            LocalFree(text.cast());
            s
        }

        #[test]
        fn the_dacl_admits_this_user_only() {
            let sid = user_sid().unwrap();
            assert!(sid.starts_with("S-1-5-"), "{sid}");
            assert_eq!(owner_only_sddl().unwrap(), format!("D:P(A;;GA;;;{sid})"));
            // Read back from a client's handle: protected, one ACE, this
            // user, every right of a file object (GA mapped to FA).
            let name = unique_name();
            let _l = Listener::bind(&name).unwrap();
            let client = open(&name).unwrap();
            // SAFETY: a handle we hold; the descriptor and its string are
            // LocalAlloc'd for us and freed here.
            let dacl = unsafe {
                let mut sd: PSECURITY_DESCRIPTOR = std::ptr::null_mut();
                let null = std::ptr::null_mut();
                let e = GetSecurityInfo(
                    client.as_raw_handle(),
                    SE_KERNEL_OBJECT,
                    DACL_SECURITY_INFORMATION,
                    null,
                    null,
                    std::ptr::null_mut(),
                    std::ptr::null_mut(),
                    &mut sd,
                );
                assert_eq!(e, 0, "GetSecurityInfo: {}", std::io::Error::from_raw_os_error(e as i32));
                let s = dacl_sddl(sd);
                LocalFree(sd);
                s
            };
            // SDDL prints a well-known account by its alias (the built-in
            // Administrator, which a CI runner runs as, prints as LA), so
            // the expected DACL is rendered through SDDL too.
            let want = Descriptor::from_sddl(&format!("D:P(A;;FA;;;{sid})")).unwrap();
            // SAFETY: a descriptor made from SDDL, alive for the call.
            assert_eq!(dacl, unsafe { dacl_sddl(want.0) });
        }

        /// A second serve for the same directory cannot create its pipe.
        #[test]
        fn the_pipe_has_one_server() {
            let name = unique_name();
            let first = Listener::bind(&name).unwrap();
            let e = Listener::bind(&name).err().unwrap();
            assert_eq!(e.raw_os_error(), Some(ERROR_ACCESS_DENIED as i32), "{e:?}");
            drop(first);
            // The pipe goes with its server's handles.
            let e = std::fs::OpenOptions::new().read(true).write(true).open(&name).unwrap_err();
            assert_eq!(e.raw_os_error(), Some(ERROR_FILE_NOT_FOUND as i32), "{e:?}");
        }

        /// One request per connection: the client writes, then reads the
        /// answer to its end, which the server's close marks (an end, not
        /// an error), and the next client finds a new instance. An answer
        /// the server closed on before the client read a byte arrives
        /// whole, and so does one larger than the pipe's buffer. A client
        /// that opens and closes without a request (the shell's
        /// `is_listening`) costs only its own connection.
        #[test]
        fn each_connection_answers_once_then_ends() {
            let big = "pong\n".repeat(40_000);
            let name = unique_name();
            let mut l = Listener::bind(&name).unwrap();
            let answer = big.clone();
            let (closed_tx, closed) = std::sync::mpsc::channel();
            let server = std::thread::spawn(move || {
                let mut answered = 0;
                while answered < 2 {
                    let mut c = l.accept().unwrap();
                    let mut req = [0u8; 5];
                    if c.read_exact(&mut req).is_err() {
                        continue; // opened and closed: nothing to answer
                    }
                    let reply = if &req == b"big!\n" { answer.as_str() } else { "pong\n" };
                    c.write_all(reply.as_bytes()).unwrap();
                    drop(c);
                    closed_tx.send(()).unwrap();
                    answered += 1;
                }
            });
            drop(open(&name).unwrap());
            for (req, want) in [("ping\n", "pong\n"), ("big!\n", big.as_str())] {
                let mut c = open(&name).unwrap();
                c.write_all(req.as_bytes()).unwrap();
                if want.len() < 64 * 1024 {
                    // Read only once the server has closed its end.
                    closed.recv().unwrap();
                }
                let mut got = String::new();
                c.read_to_string(&mut got).unwrap();
                assert!(got == want, "{} bytes, wanted {}", got.len(), want.len());
            }
            server.join().unwrap();
        }
    }
}
