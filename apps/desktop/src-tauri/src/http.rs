// HTTP/1.1 client over a Unix domain socket, std only.
//
// The local agent API is served on its Unix socket, and no maintained Rust
// HTTP client speaks `http+unix` without a dependency tree of its own, so
// this module implements exactly the subset the agent emits: request with
// Content-Length, response with status line, headers, and a body framed by
// Content-Length, chunked transfer-encoding, or connection close. It is a
// deliberate, bounded client — no redirects, no compression, no keep-alive.

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::time::Duration;

const MAX_HEADER_BYTES: usize = 64 * 1024;
const MAX_BODY_BYTES: usize = 256 * 1024 * 1024;

#[derive(Debug)]
pub struct HttpResponse {
    pub status: u16,
    pub body: Vec<u8>,
}

/// Perform one request against the agent. `body` is a pre-encoded JSON value.
pub fn unix_request(
    socket_path: &str,
    method: &str,
    path: &str,
    body: Option<&str>,
) -> Result<HttpResponse, String> {
    let body_bytes = body.map(str::as_bytes).unwrap_or_default();
    let mut stream = UnixStream::connect(socket_path)
        .map_err(|error| format!("cannot connect to agent socket {socket_path}: {error}"))?;
    // Long enough for migrations and log tails, short enough to notice a hang.
    let _ = stream.set_read_timeout(Some(Duration::from_secs(600)));
    let _ = stream.set_write_timeout(Some(Duration::from_secs(30)));

    let head = format!(
        "{method} {path} HTTP/1.1\r\nHost: shift-agent\r\nAccept: application/json\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n",
        body_bytes.len()
    );
    stream
        .write_all(head.as_bytes())
        .map_err(|error| format!("write request head: {error}"))?;
    if !body_bytes.is_empty() {
        stream
            .write_all(body_bytes)
            .map_err(|error| format!("write request body: {error}"))?;
    }
    stream.flush().map_err(|error| format!("flush request: {error}"))?;

    read_response(&mut stream)
}

fn read_response(stream: &mut UnixStream) -> Result<HttpResponse, String> {
    let mut buffer: Vec<u8> = Vec::with_capacity(16 * 1024);
    let mut chunk = [0u8; 8192];

    // Read until the end of the header block.
    let header_end = loop {
        if let Some(position) = find_header_end(&buffer) {
            break position;
        }
        if buffer.len() > MAX_HEADER_BYTES {
            return Err("agent response headers exceed 64 KiB".to_string());
        }
        let read = stream
            .read(&mut chunk)
            .map_err(|error| format!("read response: {error}"))?;
        if read == 0 {
            return Err("agent closed the connection before sending headers".to_string());
        }
        buffer.extend_from_slice(&chunk[..read]);
    };

    let head = String::from_utf8_lossy(&buffer[..header_end]).to_string();
    let (status, headers) = parse_head(&head)?;
    let body_start = header_end + 4;

    let chunked = header_value(&headers, "transfer-encoding")
        .map(|value| value.to_ascii_lowercase().contains("chunked"))
        .unwrap_or(false);

    if chunked {
        // Keep reading until the terminal chunk decodes cleanly.
        loop {
            if let Some(body) = decode_chunked(&buffer[body_start..])? {
                return Ok(HttpResponse { status, body });
            }
            let read = stream
                .read(&mut chunk)
                .map_err(|error| format!("read chunked response: {error}"))?;
            if read == 0 {
                return Err("agent closed the connection mid-chunk".to_string());
            }
            buffer.extend_from_slice(&chunk[..read]);
            if buffer.len() > MAX_BODY_BYTES {
                return Err("agent response exceeds 256 MiB".to_string());
            }
        }
    }

    if let Some(length) = header_value(&headers, "content-length").and_then(|v| v.trim().parse::<usize>().ok()) {
        if length > MAX_BODY_BYTES {
            return Err("agent response exceeds 256 MiB".to_string());
        }
        while buffer.len() - body_start < length {
            let read = stream
                .read(&mut chunk)
                .map_err(|error| format!("read response body: {error}"))?;
            if read == 0 {
                return Err(format!(
                    "agent response truncated: {} of {} bytes",
                    buffer.len() - body_start,
                    length
                ));
            }
            buffer.extend_from_slice(&chunk[..read]);
        }
        return Ok(HttpResponse {
            status,
            body: buffer[body_start..body_start + length].to_vec(),
        });
    }

    // No framing header: the body runs to EOF.
    loop {
        let read = match stream.read(&mut chunk) {
            Ok(read) => read,
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock || error.kind() == std::io::ErrorKind::TimedOut => {
                return Err("agent response has no length and did not close the connection".to_string());
            }
            Err(error) => return Err(format!("read response body: {error}")),
        };
        if read == 0 {
            break;
        }
        buffer.extend_from_slice(&chunk[..read]);
        if buffer.len() > MAX_BODY_BYTES {
            return Err("agent response exceeds 256 MiB".to_string());
        }
    }
    Ok(HttpResponse {
        status,
        body: buffer[body_start..].to_vec(),
    })
}

/// Locate the `\r\n\r\n` that terminates the header block.
fn find_header_end(buffer: &[u8]) -> Option<usize> {
    buffer.windows(4).position(|window| window == b"\r\n\r\n")
}

fn parse_head(head: &str) -> Result<(u16, Vec<(String, String)>), String> {
    let mut lines = head.split("\r\n");
    let status_line = lines.next().ok_or("empty response")?;
    let mut parts = status_line.splitn(3, ' ');
    let version = parts.next().unwrap_or_default();
    if !version.starts_with("HTTP/1.") {
        return Err(format!("unexpected protocol line: {status_line}"));
    }
    let status = parts
        .next()
        .and_then(|code| code.parse::<u16>().ok())
        .ok_or_else(|| format!("malformed status line: {status_line}"))?;
    let mut headers = Vec::new();
    for line in lines {
        if line.is_empty() {
            continue;
        }
        if let Some((name, value)) = line.split_once(':') {
            headers.push((name.trim().to_ascii_lowercase(), value.trim().to_string()));
        }
    }
    Ok((status, headers))
}

fn header_value<'a>(headers: &'a [(String, String)], name: &str) -> Option<&'a str> {
    headers
        .iter()
        .find(|(key, _)| key == name)
        .map(|(_, value)| value.as_str())
}

/// Decode a chunked body. Returns None while the stream is incomplete.
fn decode_chunked(input: &[u8]) -> Result<Option<Vec<u8>>, String> {
    let mut body = Vec::new();
    let mut cursor = 0usize;
    loop {
        let line_end = match find_crlf(&input[cursor..]) {
            Some(offset) => cursor + offset,
            None => return Ok(None),
        };
        let size_text = String::from_utf8_lossy(&input[cursor..line_end]);
        let size_token = size_text.split(';').next().unwrap_or("").trim();
        let size = usize::from_str_radix(size_token, 16)
            .map_err(|_| format!("malformed chunk size: {size_token:?}"))?;
        cursor = line_end + 2;
        if size == 0 {
            // Terminal chunk — the trailing CRLF (and optional trailers) may
            // still be arriving; the body itself is complete.
            return Ok(Some(body));
        }
        if input.len() < cursor + size + 2 {
            return Ok(None);
        }
        body.extend_from_slice(&input[cursor..cursor + size]);
        cursor += size;
        if &input[cursor..cursor + 2] != b"\r\n" {
            return Err("chunk is not CRLF-terminated".to_string());
        }
        cursor += 2;
    }
}

fn find_crlf(input: &[u8]) -> Option<usize> {
    input.windows(2).position(|window| window == b"\r\n")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixListener;
    use std::path::PathBuf;
    use std::sync::mpsc;
    use std::thread;

    // serve_once accepts one connection, hands the raw bytes to the test, and
    // returns what the test wants written back. Each call gets its own socket
    // file so parallel tests never collide.
    fn serve_once(name: &str, response: &'static [u8]) -> (PathBuf, mpsc::Receiver<Vec<u8>>) {
        let directory = std::env::temp_dir().join(format!("shift-desktop-test-{}", std::process::id()));
        std::fs::create_dir_all(&directory).expect("create test socket directory");
        let socket_path = directory.join(format!("{name}.sock"));
        let _ = std::fs::remove_file(&socket_path);
        let listener = UnixListener::bind(&socket_path).expect("bind test socket");
        let (sender, receiver) = mpsc::channel();
        thread::spawn(move || {
            let (connection, _) = listener.accept().expect("accept");
            let mut stream = connection;
            let mut seen = Vec::new();
            let mut chunk = [0u8; 8192];
            // Read until the client's request is complete: we know its
            // Content-Length, so read head + body.
            loop {
                let read = stream.read(&mut chunk).expect("read request");
                if read == 0 {
                    break;
                }
                seen.extend_from_slice(&chunk[..read]);
                if let Some(position) = find_header_end(&seen) {
                    let head = String::from_utf8_lossy(&seen[..position]).to_string();
                    let length = head
                        .lines()
                        .find_map(|line| {
                            let (name, value) = line.split_once(':')?;
                            if name.trim().eq_ignore_ascii_case("content-length") {
                                value.trim().parse::<usize>().ok()
                            } else {
                                None
                            }
                        })
                        .unwrap_or(0);
                    if seen.len() >= position + 4 + length {
                        break;
                    }
                }
            }
            stream.write_all(response).expect("write response");
            let _ = stream.flush();
            let _ = sender.send(seen);
        });
        (socket_path, receiver)
    }

    #[test]
    fn request_carries_method_path_and_body() {
        let (socket, received) = serve_once(
            "request-basics",
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}",
        );
        let response =
            unix_request(socket.to_str().unwrap(), "POST", "/v1/workloads", Some("{\"name\":\"a\"}"))
                .expect("request");
        assert_eq!(response.status, 200);
        assert_eq!(response.body, b"{}");
        let request = String::from_utf8(received.recv().expect("request bytes")).unwrap();
        assert!(request.starts_with("POST /v1/workloads HTTP/1.1\r\n"));
        // The body {"name":"a"} is 12 bytes, so the client must frame it with
        // exactly that Content-Length.
        assert!(request.contains("Content-Length: 12\r\n"));
        assert!(request.ends_with("{\"name\":\"a\"}"));
        let _ = std::fs::remove_file(&socket);
    }

    #[test]
    fn reads_content_length_body_in_several_writes() {
        // The body is larger than one write would typically deliver; the
        // client must keep reading until Content-Length is satisfied.
        let body = "x".repeat(200_000);
        let response_bytes = format!(
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n{}",
            body.len(),
            body
        )
        .into_bytes();
        // serve_once takes a 'static slice; leak is fine for a test.
        let response: &'static [u8] = Box::leak(response_bytes.into_boxed_slice());
        let (socket, _received) = serve_once("large-body", response);
        let result = unix_request(socket.to_str().unwrap(), "GET", "/v1/workloads", None)
            .expect("request");
        assert_eq!(result.status, 200);
        assert_eq!(result.body.len(), 200_000);
        assert!(result.body.iter().all(|byte| *byte == b'x'));
        let _ = std::fs::remove_file(&socket);
    }

    #[test]
    fn reads_chunked_body() {
        // 14 bytes in two 7-byte chunks, then the terminal chunk.
        let (socket, _received) = serve_once(
            "chunked",
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n7\r\n{\"state\r\n7\r\n\":\"ok\"}\r\n0\r\n\r\n",
        );
        let result = unix_request(socket.to_str().unwrap(), "GET", "/v1/health", None)
            .expect("request");
        assert_eq!(result.status, 200);
        assert_eq!(result.body, b"{\"state\":\"ok\"}");
        let _ = std::fs::remove_file(&socket);
    }

    #[test]
    fn surfaces_error_status_and_body() {
        let (socket, _received) = serve_once(
            "error-status",
            b"HTTP/1.1 422 Unprocessable Entity\r\nContent-Type: application/json\r\nContent-Length: 42\r\n\r\n{\"code\":\"CHECKPOINT_FAILED\",\"message\":\"x\"}",
        );
        let result = unix_request(socket.to_str().unwrap(), "POST", "/v1/checkpoints", Some("{}"))
            .expect("request");
        assert_eq!(result.status, 422);
        assert!(result.body.starts_with(b"{\"code\":\"CHECKPOINT_FAILED\""));
        let _ = std::fs::remove_file(&socket);
    }

    #[test]
    fn connection_failure_is_an_error() {
        let result = unix_request("/nonexistent/shift-agent.sock", "GET", "/v1/health", None);
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("cannot connect"));
    }
}
