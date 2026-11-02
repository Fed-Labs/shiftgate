// SHIFT desktop — Tauri shell. The window hosts a React UI (src/), while all
// network traffic flows through the Rust process: the local agent via
// commands.rs/http.rs over its Unix socket, and the control plane via the
// validated `control_request` command. The webview itself has no direct
// network access.

mod commands;
mod http;

use tauri::Manager;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .setup(|app| {
            let endpoints = commands::AppEndpoints::load(app.handle());
            app.manage(endpoints);
            app.manage(commands::ControlClient::new());
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            commands::get_config,
            commands::save_endpoints,
            commands::agent_request,
            commands::control_request,
        ])
        .run(tauri::generate_context!())
        .expect("failed to run the SHIFT desktop app");
}
