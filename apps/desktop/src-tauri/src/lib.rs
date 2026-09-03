// SHIFT desktop — Tauri shell. The window hosts a React UI (src/), while all
// agent traffic flows through the Rust process: see commands.rs and http.rs.

mod commands;
mod http;

use tauri::Manager;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_http::init())
        .setup(|app| {
            let endpoints = commands::AppEndpoints::load(app.handle());
            app.manage(endpoints);
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            commands::get_config,
            commands::save_endpoints,
            commands::agent_request,
        ])
        .run(tauri::generate_context!())
        .expect("failed to run the SHIFT desktop app");
}
