// Tauri command surface. The frontend sees exactly two data commands —
// `agent_request` (local agent over its Unix socket) and the endpoints
// config — plus the control-plane transport, which the frontend drives
// through the official HTTP plugin. Errors are strings of the form
// "<CODE>: <message>" taken verbatim from the agent's own error responses,
// so a rejected request always carries its real reason to the UI.

use crate::http;
use serde::Serialize;
use std::fs;
use std::path::PathBuf;
use std::sync::Mutex;
use tauri::{AppHandle, Manager, State};

pub const DEFAULT_AGENT_SOCKET: &str = "/run/shift/agent.sock";

#[derive(Clone, Serialize)]
pub struct Endpoints {
    pub control_plane_url: String,
    pub agent_socket: String,
}

pub struct AppEndpoints(pub Mutex<Endpoints>);

impl AppEndpoints {
    pub fn load(app: &AppHandle) -> Self {
        let defaults = Endpoints {
            control_plane_url: String::new(),
            agent_socket: std::env::var("SHIFT_AGENT_ENDPOINT")
                .ok()
                .filter(|value| !value.trim().is_empty())
                .unwrap_or_else(|| DEFAULT_AGENT_SOCKET.to_string()),
        };
        match config_path(app) {
            Some(path) => match fs::read_to_string(&path) {
                Ok(text) => match serde_json::from_str::<serde_json::Value>(&text) {
                    Ok(value) => Self(Mutex::new(Endpoints {
                        control_plane_url: value
                            .get("control_plane_url")
                            .and_then(|v| v.as_str())
                            .unwrap_or(defaults.control_plane_url.as_str())
                            .to_string(),
                        agent_socket: value
                            .get("agent_socket")
                            .and_then(|v| v.as_str())
                            .filter(|v| !v.trim().is_empty())
                            .unwrap_or(defaults.agent_socket.as_str())
                            .to_string(),
                    })),
                    Err(_) => Self(Mutex::new(defaults)),
                },
                Err(_) => Self(Mutex::new(defaults)),
            },
            None => Self(Mutex::new(defaults)),
        }
    }
}

fn config_path(app: &AppHandle) -> Option<PathBuf> {
    app.path().app_config_dir().ok().map(|dir| dir.join("endpoints.json"))
}

#[tauri::command]
pub fn get_config(endpoints: State<AppEndpoints>) -> Result<Endpoints, String> {
    let current = endpoints
        .0
        .lock()
        .map_err(|_| "endpoint state poisoned".to_string())?;
    Ok(current.clone())
}

#[tauri::command]
pub fn save_endpoints(
    app: AppHandle,
    endpoints: State<AppEndpoints>,
    control_plane_url: String,
    agent_socket: String,
) -> Result<Endpoints, String> {
    let agent_socket = if agent_socket.trim().is_empty() {
        DEFAULT_AGENT_SOCKET.to_string()
    } else {
        agent_socket.trim().to_string()
    };
    let control_plane_url = control_plane_url.trim().trim_end_matches('/').to_string();
    let next = Endpoints {
        control_plane_url: control_plane_url.clone(),
        agent_socket: agent_socket.clone(),
    };
    if let Some(path) = config_path(&app) {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)
                .map_err(|error| format!("create config directory: {error}"))?;
        }
        let document = serde_json::json!({
            "control_plane_url": control_plane_url,
            "agent_socket": agent_socket,
        });
        let text = serde_json::to_string_pretty(&document)
            .map_err(|error| format!("encode endpoints: {error}"))?;
        fs::write(&path, text).map_err(|error| format!("persist endpoints: {error}"))?;
    }
    let mut current = endpoints
        .0
        .lock()
        .map_err(|_| "endpoint state poisoned".to_string())?;
    *current = next.clone();
    Ok(next)
}

/// One request to the local agent. Returns the raw body as a string; the
/// frontend parses it against the typed agent model. Non-2xx responses map to
/// Err("<CODE>: <message>") using the agent's own error payload.
#[tauri::command]
pub async fn agent_request(
    endpoints: State<'_, AppEndpoints>,
    method: String,
    path: String,
    body: Option<serde_json::Value>,
) -> Result<String, String> {
    if !path.starts_with('/') {
        return Err(format!("INVALID_PATH: agent request path must start with '/': {path}"));
    }
    let socket = {
        let current = endpoints
            .0
            .lock()
            .map_err(|_| "ENDPOINT_STATE: endpoint state poisoned".to_string())?;
        current.agent_socket.clone()
    };
    let encoded = match body {
        Some(value) => Some(
            serde_json::to_string(&value).map_err(|error| format!("ENCODE_BODY: {error}"))?,
        ),
        None => None,
    };
    let method = method.to_ascii_uppercase();
    let response = tauri::async_runtime::spawn_blocking(move || {
        http::unix_request(&socket, &method, &path, encoded.as_deref())
    })
    .await
    .map_err(|error| format!("AGENT_REQUEST: {error}"))?
    .map_err(|message| format!("AGENT_UNREACHABLE: {message}"))?;

    if (200..300).contains(&response.status) {
        return Ok(String::from_utf8_lossy(&response.body).to_string());
    }
    // Error bodies carry {code, message} — surface them verbatim.
    let text = String::from_utf8_lossy(&response.body).to_string();
    let detail = serde_json::from_str::<serde_json::Value>(&text)
        .ok()
        .and_then(|value| {
            let code = value.get("code").and_then(|c| c.as_str()).unwrap_or("AGENT_ERROR");
            let message = value
                .get("message")
                .and_then(|m| m.as_str())
                .unwrap_or("the agent rejected the request");
            Some(format!("{code}: {message}"))
        })
        .unwrap_or_else(|| {
            if text.trim().is_empty() {
                format!("AGENT_HTTP_{}: the agent rejected the request", response.status)
            } else {
                format!("AGENT_HTTP_{}: {}", response.status, text.trim())
            }
        });
    Err(detail)
}
