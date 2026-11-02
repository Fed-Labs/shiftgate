// Tauri command surface. The frontend sees exactly three data commands —
// `agent_request` (local agent over its Unix socket), `control_request`
// (control plane over HTTPS, validated against the configured endpoint), and
// the endpoints config. Errors are strings of the form "<CODE>: <message>"
// taken verbatim from the agent's own error responses, so a rejected request
// always carries its real reason to the UI.
//
// Every command is declared with `rename_all = "snake_case"`, so the invoke
// argument keys are the parameter names verbatim (`control_plane_url`,
// `agent_socket`, …) — the same spelling the API, the config file, and the
// TypeScript models use. The default camelCase renaming once renamed
// `control_plane_url` to `controlPlaneUrl`, silently breaking every save.

use crate::http;
use serde::Serialize;
use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;
use std::sync::Mutex;
use tauri::{AppHandle, Manager, State};

pub const DEFAULT_AGENT_SOCKET: &str = "/run/shift/agent.sock";
/// The platform's control plane — the same endpoint the CLI defaults to.
pub const DEFAULT_CONTROL_URL: &str = "https://shiftgate.dev";
/// The agent caps local request bodies at 4 MiB (internal/agent/api.go);
/// the webview side honors the same limit.
pub const MAX_REQUEST_BYTES: usize = 4 * 1024 * 1024;
/// Control-plane responses (fleet listings with capabilities) are larger than
/// agent responses but still bounded.
pub const MAX_CONTROL_RESPONSE_BYTES: usize = 32 * 1024 * 1024;
/// Headers the webview may attach to a control-plane request. Everything else
/// (Host, Cookie, …) is the transport's business, not the caller's.
const ALLOWED_CONTROL_HEADERS: [&str; 3] = ["content-type", "authorization", "accept"];

#[derive(Clone, Serialize)]
pub struct Endpoints {
    pub control_plane_url: String,
    pub agent_socket: String,
    /// Set when the persisted config could not be read or parsed and the app
    /// fell back to defaults. Surfaced in Settings instead of failing silently
    /// back to the platform control plane.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub load_error: Option<String>,
}

pub struct AppEndpoints(pub Mutex<Endpoints>);

impl AppEndpoints {
    pub fn load<R: tauri::Runtime>(app: &AppHandle<R>) -> Self {
        let defaults = Endpoints {
            control_plane_url: String::new(),
            agent_socket: std::env::var("SHIFT_AGENT_ENDPOINT")
                .ok()
                .filter(|value| !value.trim().is_empty())
                .unwrap_or_else(|| DEFAULT_AGENT_SOCKET.to_string()),
            load_error: None,
        };
        let Some(path) = config_path(app) else {
            return Self(Mutex::new(defaults));
        };
        match fs::read_to_string(&path) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                // No saved configuration is the normal first-run state.
                Self(Mutex::new(defaults))
            }
            Err(error) => Self(Mutex::new(Endpoints {
                load_error: Some(format!(
                    "endpoints.json could not be read ({error}) — using the default endpoints"
                )),
                ..defaults
            })),
            Ok(text) => match serde_json::from_str::<serde_json::Value>(&text) {
                Err(error) => Self(Mutex::new(Endpoints {
                    load_error: Some(format!(
                        "endpoints.json is not valid JSON ({error}) — using the default endpoints"
                    )),
                    ..defaults
                })),
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
                    load_error: None,
                })),
            },
        }
    }
}

fn config_path<R: tauri::Runtime>(app: &AppHandle<R>) -> Option<PathBuf> {
    app.path()
        .app_config_dir()
        .ok()
        .map(|dir| dir.join("endpoints.json"))
}

#[tauri::command(rename_all = "snake_case")]
pub fn get_config(endpoints: State<AppEndpoints>) -> Result<Endpoints, String> {
    let current = endpoints
        .0
        .lock()
        .map_err(|_| "ENDPOINT_STATE: endpoint state poisoned".to_string())?;
    Ok(current.clone())
}

#[tauri::command(rename_all = "snake_case")]
pub fn save_endpoints<R: tauri::Runtime>(
    app: AppHandle<R>,
    endpoints: State<AppEndpoints>,
    control_plane_url: String,
    agent_socket: String,
) -> Result<Endpoints, String> {
    let (control_plane_url, agent_socket) = normalize_endpoints(&control_plane_url, &agent_socket)?;
    let next = Endpoints {
        control_plane_url: control_plane_url.clone(),
        agent_socket: agent_socket.clone(),
        load_error: None,
    };
    if let Some(path) = config_path(&app) {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)
                .map_err(|error| format!("CONFIG_WRITE: create config directory: {error}"))?;
        }
        let document = serde_json::json!({
            "control_plane_url": control_plane_url,
            "agent_socket": agent_socket,
        });
        let text = serde_json::to_string_pretty(&document)
            .map_err(|error| format!("CONFIG_WRITE: encode endpoints: {error}"))?;
        write_config_atomic(&path, &text)?;
    }
    let mut current = endpoints
        .0
        .lock()
        .map_err(|_| "ENDPOINT_STATE: endpoint state poisoned".to_string())?;
    *current = next.clone();
    Ok(next)
}

/// Write the config via a temp file + rename so a crash or disk-full can
/// never leave a truncated `endpoints.json` that silently reverts the app to
/// the platform control plane. The file is owner-only (0600).
fn write_config_atomic(path: &PathBuf, text: &str) -> Result<(), String> {
    let temporary = path.with_extension("json.tmp");
    fs::write(&temporary, text)
        .map_err(|error| format!("CONFIG_WRITE: persist endpoints: {error}"))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(&temporary, fs::Permissions::from_mode(0o600));
    }
    fs::rename(&temporary, path).map_err(|error| {
        let _ = fs::remove_file(&temporary);
        format!("CONFIG_WRITE: activate endpoints: {error}")
    })?;
    Ok(())
}

/// Validate and normalize a configuration change at the point of the mistake:
/// a control-plane URL must be https (plain http only for loopback hosts) and
/// the agent socket must be an absolute, printable path.
fn normalize_endpoints(
    control_plane_url: &str,
    agent_socket: &str,
) -> Result<(String, String), String> {
    let control_plane_url = control_plane_url.trim().trim_end_matches('/').to_string();
    if !control_plane_url.is_empty() {
        validate_control_url(&control_plane_url)?;
    }
    let agent_socket = if agent_socket.trim().is_empty() {
        DEFAULT_AGENT_SOCKET.to_string()
    } else {
        let socket = agent_socket.trim();
        if !socket.starts_with('/') {
            return Err(format!(
                "AGENT_SOCKET_INVALID: the agent socket path must be absolute: {socket}"
            ));
        }
        if socket
            .bytes()
            .any(|byte| byte == 0 || byte < 0x20 || byte == 0x7f)
        {
            return Err(
                "AGENT_SOCKET_INVALID: the agent socket path contains control bytes".to_string(),
            );
        }
        // sun_path is 108 bytes including the NUL terminator on Linux.
        if socket.len() > 107 {
            return Err(format!(
                "AGENT_SOCKET_INVALID: the agent socket path is too long ({} bytes; the kernel limit is 107)",
                socket.len()
            ));
        }
        socket.to_string()
    };
    Ok((control_plane_url, agent_socket))
}

fn validate_control_url(url_text: &str) -> Result<(), String> {
    let url = reqwest::Url::parse(url_text)
        .map_err(|error| format!("CONTROL_URL_INVALID: not a valid URL: {error}"))?;
    let scheme = url.scheme().to_string();
    if scheme != "https" && scheme != "http" {
        return Err(format!(
            "CONTROL_URL_INVALID: the control plane URL must use https (plain http only for loopback), not {scheme}"
        ));
    }
    let host = url
        .host_str()
        .ok_or_else(|| "CONTROL_URL_INVALID: the control plane URL has no host".to_string())?;
    if !url.username().is_empty() || url.password().is_some() {
        return Err(
            "CONTROL_URL_INVALID: the control plane URL must not embed credentials".to_string(),
        );
    }
    if scheme == "http" && !is_loopback_host(host) {
        return Err(format!(
            "CONTROL_URL_INVALID: plain http is only allowed for loopback control planes (got host {host}) — use https"
        ));
    }
    Ok(())
}

fn is_loopback_host(host: &str) -> bool {
    let bare = host.trim_start_matches('[').trim_end_matches(']');
    if bare.eq_ignore_ascii_case("localhost") {
        return true;
    }
    bare.parse::<std::net::IpAddr>()
        .map(|address| address.is_loopback())
        .unwrap_or(false)
}

/// One request to the local agent. Returns the raw body as a string; the
/// frontend parses it against the typed agent model. Non-2xx responses map to
/// Err("<CODE>: <message>") using the agent's own error payload.
#[tauri::command(rename_all = "snake_case")]
pub async fn agent_request(
    endpoints: State<'_, AppEndpoints>,
    method: String,
    path: String,
    body: Option<serde_json::Value>,
) -> Result<String, String> {
    let method = method.to_ascii_uppercase();
    if !valid_method(&method) {
        return Err(format!(
            "INVALID_METHOD: agent request method must be an HTTP token ([A-Z]+): {method}"
        ));
    }
    if !valid_path(&path) {
        return Err(format!(
            "INVALID_PATH: agent request path must start with '/' and contain no spaces, fragments, or control bytes: {path}"
        ));
    }
    let socket = {
        let current = endpoints
            .0
            .lock()
            .map_err(|_| "ENDPOINT_STATE: endpoint state poisoned".to_string())?;
        current.agent_socket.clone()
    };
    let encoded = encode_body(body)?;
    let response = tauri::async_runtime::spawn_blocking(move || {
        http::unix_request(&socket, &method, &path, encoded.as_deref())
    })
    .await
    .map_err(|error| format!("AGENT_REQUEST: {error}"))?
    .map_err(|error| match error {
        http::RequestError::Unreachable(message) => format!("AGENT_UNREACHABLE: {message}"),
        http::RequestError::Timeout(message) => format!("AGENT_TIMEOUT: {message}"),
        http::RequestError::Protocol(message) => format!("AGENT_PROTOCOL: {message}"),
    })?;

    if (200..300).contains(&response.status) {
        return Ok(String::from_utf8_lossy(&response.body).to_string());
    }
    // Error bodies carry {code, message} — surface them verbatim.
    let text = String::from_utf8_lossy(&response.body).to_string();
    let detail = serde_json::from_str::<serde_json::Value>(&text)
        .ok()
        .map(|value| {
            let code = value
                .get("code")
                .and_then(|c| c.as_str())
                .unwrap_or("AGENT_ERROR");
            let message = value
                .get("message")
                .and_then(|m| m.as_str())
                .unwrap_or("the agent rejected the request");
            format!("{code}: {message}")
        })
        .unwrap_or_else(|| {
            let summary = summarize_body(&text);
            if summary.is_empty() {
                format!(
                    "AGENT_HTTP_{}: the agent rejected the request",
                    response.status
                )
            } else {
                format!("AGENT_HTTP_{}: {}", response.status, summary)
            }
        });
    Err(detail)
}

#[derive(Serialize)]
pub struct ControlResponse {
    pub status: u16,
    pub body: String,
}

/// The control-plane transport. The webview never holds raw HTTP: it names a
/// path, and this command performs the request against the configured control
/// plane only — the target cannot be redirected to any other host. HTTPS is
/// verified against the built-in root store; plain http is accepted only for
/// loopback control planes.
pub struct ControlClient(pub reqwest::Client);

impl ControlClient {
    pub fn new() -> Self {
        let client = reqwest::Client::builder()
            .connect_timeout(std::time::Duration::from_secs(10))
            .timeout(std::time::Duration::from_secs(30))
            // A redirect to another host would bypass the target validation
            // below; the control plane must answer directly.
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .unwrap_or_default();
        Self(client)
    }
}

#[tauri::command(rename_all = "snake_case")]
pub async fn control_request(
    endpoints: State<'_, AppEndpoints>,
    client: State<'_, ControlClient>,
    method: String,
    path: String,
    body: Option<serde_json::Value>,
    headers: Option<HashMap<String, String>>,
) -> Result<ControlResponse, String> {
    let method = method.to_ascii_uppercase();
    if !valid_method(&method) {
        return Err(format!(
            "INVALID_METHOD: control-plane request method must be an HTTP token ([A-Z]+): {method}"
        ));
    }
    if !valid_path(&path) {
        return Err(format!(
            "INVALID_PATH: control-plane request path must start with '/' and contain no spaces, fragments, or control bytes: {path}"
        ));
    }
    let base = {
        let current = endpoints
            .0
            .lock()
            .map_err(|_| "ENDPOINT_STATE: endpoint state poisoned".to_string())?;
        if current.control_plane_url.is_empty() {
            DEFAULT_CONTROL_URL.to_string()
        } else {
            current.control_plane_url.trim_end_matches('/').to_string()
        }
    };
    let target = format!("{base}{path}");
    validate_control_url(&target)?;

    let http_method = reqwest::Method::from_bytes(method.as_bytes())
        .map_err(|error| format!("INVALID_METHOD: {error}"))?;
    let mut request = client.0.request(http_method, &target);
    if let Some(headers) = headers {
        for (name, value) in headers {
            let lowered = name.to_ascii_lowercase();
            if !ALLOWED_CONTROL_HEADERS.contains(&lowered.as_str()) {
                return Err(format!(
                    "CONTROL_HEADER_INVALID: header {lowered} may not be set on control-plane requests"
                ));
            }
            let name = reqwest::header::HeaderName::from_bytes(lowered.as_bytes())
                .map_err(|error| format!("CONTROL_HEADER_INVALID: {error}"))?;
            let value = reqwest::header::HeaderValue::from_str(&value)
                .map_err(|error| format!("CONTROL_HEADER_INVALID: {error}"))?;
            request = request.header(name, value);
        }
    }
    if let Some(encoded) = encode_body(body)? {
        request = request.body(encoded);
    }

    let response = request
        .send()
        .await
        .map_err(|error| format!("CONTROL_TRANSPORT: {error}"))?;
    let status = response.status().as_u16();
    let mut body: Vec<u8> = Vec::new();
    let mut response = response;
    while let Some(chunk) = response
        .chunk()
        .await
        .map_err(|error| format!("CONTROL_TRANSPORT: read response: {error}"))?
    {
        if body.len() + chunk.len() > MAX_CONTROL_RESPONSE_BYTES {
            return Err(
                "CONTROL_RESPONSE_TOO_LARGE: the control plane response exceeds 32 MiB".to_string(),
            );
        }
        body.extend_from_slice(&chunk);
    }
    Ok(ControlResponse {
        status,
        body: String::from_utf8_lossy(&body).to_string(),
    })
}

/// Encode a JSON request body within the agent's own 4 MiB cap.
fn encode_body(body: Option<serde_json::Value>) -> Result<Option<String>, String> {
    match body {
        None => Ok(None),
        Some(value) => {
            let text =
                serde_json::to_string(&value).map_err(|error| format!("ENCODE_BODY: {error}"))?;
            if text.len() > MAX_REQUEST_BYTES {
                return Err(format!(
                    "REQUEST_TOO_LARGE: request body exceeds 4 MiB ({} bytes)",
                    text.len()
                ));
            }
            Ok(Some(text))
        }
    }
}

/// First line of a non-JSON error body, control bytes scrubbed and length
/// capped — the UI shows a reason, never a raw proxy/HTML blob.
fn summarize_body(text: &str) -> String {
    let first = text.lines().next().unwrap_or("").trim();
    let scrubbed: String = first
        .chars()
        .map(|character| {
            if character.is_control() {
                ' '
            } else {
                character
            }
        })
        .collect();
    if scrubbed.chars().count() > 200 {
        let truncated: String = scrubbed.chars().take(200).collect();
        format!("{truncated}…")
    } else {
        scrubbed
    }
}

/// The request head is assembled with raw string interpolation, so a space or
/// CR/LF in the method or path would smuggle arbitrary bytes onto the agent
/// socket. Methods are ASCII-uppercase tokens; paths are RFC 3986
/// pchar/`/`/`?` characters only — no spaces, no fragments, no control bytes.
fn valid_method(method: &str) -> bool {
    !method.is_empty() && method.bytes().all(|byte| byte.is_ascii_uppercase())
}

fn valid_path(path: &str) -> bool {
    path.starts_with('/')
        && path.chars().all(|character| {
            character.is_ascii_alphanumeric()
                || matches!(
                    character,
                    '-' | '_'
                        | '.'
                        | '~'
                        | ':'
                        | '/'
                        | '?'
                        | '['
                        | ']'
                        | '@'
                        | '!'
                        | '$'
                        | '&'
                        | '\''
                        | '('
                        | ')'
                        | '*'
                        | '+'
                        | ','
                        | ';'
                        | '='
                        | '%'
                )
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_smuggled_method() {
        assert!(!valid_method("GET /v1/health HTTP/1.1\r\n\r\nPOST"));
        assert!(!valid_method(""));
        assert!(!valid_method("get"));
        assert!(!valid_method("GET\r\nX: y"));
    }

    #[test]
    fn rejects_crlf_and_space_in_path() {
        assert!(!valid_path(
            "/v1/health HTTP/1.1\r\n\r\nPOST /v1/workloads "
        ));
        assert!(!valid_path("/v1/health\r\n"));
        assert!(!valid_path("/v1/health\n"));
        assert!(!valid_path("v1/health"));
    }

    #[test]
    fn rejects_fragment_in_path() {
        // A fragment is not a legal request-target; '#' must never reach the
        // request head.
        assert!(!valid_path("/v1/health#x"));
        assert!(!valid_path("/v1/workloads#"));
    }

    #[test]
    fn accepts_real_agent_paths() {
        assert!(valid_method("GET"));
        assert!(valid_method("POST"));
        assert!(valid_method("DELETE"));
        assert!(valid_path("/v1/health"));
        assert!(valid_path("/v1/checkpoints?workload_id=abc-123"));
        assert!(valid_path("/v1/workloads/123e4567-e89b/logs?tail=100"));
        assert!(valid_path(
            "/v1/organizations/abc/machines?limit=500&offset=0"
        ));
    }

    #[test]
    fn accepts_https_and_loopback_http_control_urls() {
        assert!(validate_control_url("https://shiftgate.dev").is_ok());
        assert!(validate_control_url("https://control.internal:8443").is_ok());
        assert!(validate_control_url("http://localhost:8080").is_ok());
        assert!(validate_control_url("http://127.0.0.1:3000").is_ok());
        assert!(validate_control_url("http://[::1]:3000").is_ok());
    }

    #[test]
    fn rejects_non_https_remote_and_exotic_control_urls() {
        assert!(validate_control_url("http://control.internal:8443").is_err());
        assert!(validate_control_url("javascript:alert(1)").is_err());
        assert!(validate_control_url("file:///etc/passwd").is_err());
        assert!(validate_control_url("not a url").is_err());
        assert!(validate_control_url("https://user:pass@control.internal").is_err());
        assert!(validate_control_url("ftp://control.internal").is_err());
    }

    #[test]
    fn normalizes_endpoints_and_validates_the_socket() {
        let (url, socket) =
            normalize_endpoints("https://shiftgate.dev/", " /run/shift/agent.sock ")
                .expect("valid");
        assert_eq!(url, "https://shiftgate.dev");
        assert_eq!(socket, "/run/shift/agent.sock");

        let (_, socket) = normalize_endpoints("", "").expect("defaults");
        assert_eq!(socket, DEFAULT_AGENT_SOCKET);

        // Empty URL means "the platform default", which is https.
        assert!(normalize_endpoints("", "/run/shift/agent.sock").is_ok());
    }

    #[test]
    fn rejects_bad_sockets_at_save_time() {
        assert!(normalize_endpoints("", "run/shift/agent.sock").is_err());
        assert!(normalize_endpoints("", "/run/shift/agent\0.sock").is_err());
        assert!(normalize_endpoints("", &format!("/run/{}", "a".repeat(200))).is_err());
        assert!(normalize_endpoints("http://public.host", "/run/shift/agent.sock").is_err());
    }

    #[test]
    fn request_body_over_the_cap_is_refused() {
        let big = serde_json::Value::String("x".repeat(MAX_REQUEST_BYTES));
        let error = encode_body(Some(big)).expect_err("oversized body must be refused");
        assert!(error.contains("REQUEST_TOO_LARGE"));
        assert!(encode_body(Some(serde_json::json!({"name": "a"}))).is_ok());
        assert!(encode_body(None).is_ok());
    }

    #[test]
    fn non_json_error_bodies_are_summarized_not_echoed() {
        assert_eq!(
            summarize_body("<html>\n<body>Bad gateway</body></html>"),
            "<html>"
        );
        assert_eq!(summarize_body("plain error"), "plain error");
        assert_eq!(summarize_body("   \n"), "");
        let long = "y".repeat(500);
        let summary = summarize_body(&long);
        assert_eq!(summary.chars().count(), 201);
        assert!(summary.ends_with('…'));
    }

    /// End-to-end invoke-boundary check: the shipped argument names must work
    /// through Tauri's real command dispatch. The old default camelCase rename
    /// turned `control_plane_url` into `controlPlaneUrl`, so every
    /// `save_endpoints` invoke reported a missing key and Settings save and
    /// custom-URL sign-in always failed while `cargo test` stayed green.
    #[test]
    fn save_endpoints_accepts_the_shipped_snake_case_invoke_keys() {
        let config_home =
            std::env::temp_dir().join(format!("shift-desktop-ipc-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&config_home);
        std::fs::create_dir_all(&config_home).expect("create config home");
        // Redirect app_config_dir so the persisted endpoints.json lands in a
        // scratch directory instead of the user's real config.
        std::env::set_var("XDG_CONFIG_HOME", &config_home);

        let app = tauri::test::mock_builder()
            .invoke_handler(tauri::generate_handler![
                get_config,
                save_endpoints,
                agent_request,
                control_request,
            ])
            .build(tauri::test::mock_context(tauri::test::noop_assets()))
            .expect("build mock app");
        app.manage(AppEndpoints(Mutex::new(Endpoints {
            control_plane_url: String::new(),
            agent_socket: DEFAULT_AGENT_SOCKET.to_string(),
            load_error: None,
        })));
        app.manage(ControlClient::new());
        let webview = tauri::WebviewWindowBuilder::new(&app, "main", Default::default())
            .build()
            .expect("build mock webview");

        let invoke = |cmd: &str, args: serde_json::Value| tauri::webview::InvokeRequest {
            cmd: cmd.to_string(),
            callback: tauri::ipc::CallbackFn(0),
            error: tauri::ipc::CallbackFn(1),
            url: "tauri://localhost".parse().expect("invoke url"),
            body: tauri::ipc::InvokeBody::Json(args),
            headers: Default::default(),
            invoke_key: tauri::test::INVOKE_KEY.to_string(),
        };

        // The keys the frontend actually sends (lib/backend.ts) must succeed.
        let ok = tauri::test::get_ipc_response(
            &webview,
            invoke(
                "save_endpoints",
                serde_json::json!({
                    "control_plane_url": "https://control.test",
                    "agent_socket": "/tmp/shift-ipc-test.sock",
                }),
            ),
        )
        .expect("save_endpoints with snake_case keys must succeed");
        let saved: serde_json::Value = ok.deserialize().expect("decode endpoints");
        assert_eq!(saved["control_plane_url"], "https://control.test");
        assert_eq!(saved["agent_socket"], "/tmp/shift-ipc-test.sock");

        // The old camelCase spellings must fail: the arg names are the
        // snake_case parameter names verbatim.
        let stale = tauri::test::get_ipc_response(
            &webview,
            invoke(
                "save_endpoints",
                serde_json::json!({
                    "controlPlaneURL": "https://control.test",
                    "agentSocket": "/tmp/shift-ipc-test.sock",
                }),
            ),
        );
        assert!(stale.is_err(), "camelCase keys must not resolve");

        let _ = std::fs::remove_dir_all(&config_home);
    }

    /// Locks the frontend half of the invoke contract: the shipped
    /// `save_endpoints` call must keep passing the snake_case keys. Drift on
    /// either side of the boundary is what made every Settings save fail.
    #[test]
    fn frontend_invokes_save_endpoints_with_snake_case_keys() {
        let source = std::fs::read_to_string(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../src/lib/backend.ts"),
        )
        .expect("read backend.ts");
        let call_start = source
            .find("invoke<BackendConfig>(\"save_endpoints\"")
            .expect("backend.ts must invoke save_endpoints");
        let call = &source[call_start
            ..source[call_start..]
                .find(')')
                .map(|i| call_start + i + 1)
                .unwrap_or(source.len())];
        assert!(
            call.contains("control_plane_url: controlPlaneURL"),
            "save_endpoints must send the control_plane_url key: {call}"
        );
        assert!(
            call.contains("agent_socket: agentSocket"),
            "save_endpoints must send the agent_socket key: {call}"
        );
        assert!(
            !call.contains("{ controlPlaneURL"),
            "save_endpoints must not send camelCase invoke keys: {call}"
        );
    }
}
