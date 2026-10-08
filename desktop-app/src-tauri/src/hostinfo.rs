//! Facts about this computer that Settings shows and the memory check
//! uses: total memory and CPUs.

/// Total physical memory in MiB.
pub fn memory_mb() -> u64 {
    total_bytes().map(|b| b / (1024 * 1024)).unwrap_or(16 * 1024)
}

pub fn cpus() -> u64 {
    std::thread::available_parallelism().map(|n| n.get() as u64).unwrap_or(4)
}

#[cfg(target_os = "macos")]
fn total_bytes() -> Option<u64> {
    let mut v: u64 = 0;
    let mut len = std::mem::size_of::<u64>();
    // SAFETY: sysctlbyname into a correctly sized u64.
    let r = unsafe {
        libc::sysctlbyname(c"hw.memsize".as_ptr(), &mut v as *mut u64 as *mut libc::c_void, &mut len, std::ptr::null_mut(), 0)
    };
    (r == 0 && v > 0).then_some(v)
}

#[cfg(target_os = "linux")]
fn total_bytes() -> Option<u64> {
    let text = std::fs::read_to_string("/proc/meminfo").ok()?;
    let kb: u64 = text.lines().find(|l| l.starts_with("MemTotal:"))?.split_whitespace().nth(1)?.parse().ok()?;
    Some(kb * 1024)
}

#[cfg(windows)]
fn total_bytes() -> Option<u64> {
    use windows_sys::Win32::System::SystemInformation::{GlobalMemoryStatusEx, MEMORYSTATUSEX};
    // SAFETY: a zeroed MEMORYSTATUSEX with its length set, as the API requires.
    unsafe {
        let mut m: MEMORYSTATUSEX = std::mem::zeroed();
        m.dwLength = std::mem::size_of::<MEMORYSTATUSEX>() as u32;
        (GlobalMemoryStatusEx(&mut m) != 0).then_some(m.ullTotalPhys)
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn this_computer_has_memory_and_cpus() {
        assert!(super::memory_mb() >= 1024);
        assert!(super::cpus() >= 1);
    }
}
