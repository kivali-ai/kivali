// Keeps a console window from opening alongside the app on Windows. Such
// a program starts with no console and no standard handles: the shell
// logs to a file (lib.rs, `log_to_file`) and a flag attaches to the
// console it was run from (cli.rs).
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    if let Some(code) = kivali_desktop_lib::cli::answer(std::env::args().skip(1)) {
        std::process::exit(code);
    }
    kivali_desktop_lib::run();
}
