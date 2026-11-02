// HTTP/1.1 client over a Unix domain socket, std only.
//
// The local agent API is served on its Unix socket, and no maintained Rust
// HTTP client speaks `http+unix` without a dependency tree of its own, so
// this module implements exactly the subset the agent emits: request with
// Content-Length, response with status line, headers, and a body framed by
// Content-Length, chunked transfer-encoding, or connection close. It is a
// deliberate, bounded client — no redirects, no compression, no keep-alive.
//
// Every request carries a connect deadline and a total deadline, so a wedged
// or slow-dripping agent can never pin a request (or the UI behind it)
// indefinitely. Failure classes are distinguished — reachability, timeout,
// protocol — so the UI can say what actually went wrong.

use std::io::{Read, Write};
use std::time::{Duration, Instant};

const MAX_HEADER_BYTES: usize = 64 * 1024;
const MAX_BODY_BYTES: usize = 256 * 1024 * 1024;

/// Bounds for ordinary round trips: health polls, listings, quick mutations.
const SHORT_TOTAL: Duration = Duration::from_secs(30);
/// Bounds for routes that legitimately run long: checkpoint capture, restore,
/// migration orchestration, log tails, update feed checks.
const LONG_TOTAL: Duration = Duration::from_secs(600);
/// A Unix listener with a full backlog makes `connect` block; never forever.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
/// The request body is small, local, and pre-encoded; a wedged write is a
/// dead agent, not a slow one.
const WRITE_TIMEOUT: Duration = Duration::from_secs(30);

#[derive(Debug)]
pub struct HttpResponse {
    pub status: u16,
    pub body: Vec<u8>,
}

/// Why a request failed. Reachability (socket refused/absent), a blown
/// deadline, and a malformed or hostile response are different problems and
/// map to different error codes for the UI.
#[derive(Debug)]
pub enum RequestError {
    Unreachable(String),
    Timeout(String),
    Protocol(String),
}

impl std::fmt::Display for RequestError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RequestError::Unreachable(message) => write!(formatter, "{message}"),
            RequestError::Timeout(message) => write!(formatter, "{message}"),
            RequestError::Protocol(message) => write!(formatter, "{message}"),
        }
    }
}

pub type Result<T> = std::result::Result<T, RequestError>;

/// How long one request may run end to end, keyed by route class.
pub fn total_deadline(path: &str) -> Duration {
    if path.starts_with("/v1/checkpoints")
        || path.starts_with("/v1/migrations")
        || path.starts_with("/v1/restores")
        || path.starts_with("/v1/updates")
        || path.starts_with("/v1/gc")
        || path.contains("/logs")
        || path.contains("/fork")
        || path.contains("/failover")
    {
        LONG_TOTAL
    } else {
        SHORT_TOTAL
    }
}

/// Perform one request against the agent. `body` is a pre-encoded JSON value.
#[cfg(unix)]
pub fn unix_request(
    socket_path: &str,
    method: &str,
    path: &str,
    body: Option<&str>,
) -> Result<HttpResponse> {
    let body_bytes = body.map(str::as_bytes).unwrap_or_default();
    let mut stream = connect_with_deadline(socket_path, CONNECT_TIMEOUT)?;
    let deadline = Instant::now() + total_deadline(path);
    let _ = stream.set_write_timeout(Some(WRITE_TIMEOUT));

    let head = format!(
        "{method} {path} HTTP/1.1\r\nHost: shift-agent\r\nAccept: application/json\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n",
        body_bytes.len()
    );
    stream
        .write_all(head.as_bytes())
        .map_err(|error| RequestError::Unreachable(format!("write request head: {error}")))?;
    if !body_bytes.is_empty() {
        stream
            .write_all(body_bytes)
            .map_err(|error| RequestError::Unreachable(format!("write request body: {error}")))?;
    }
    stream
        .flush()
        .map_err(|error| RequestError::Unreachable(format!("flush request: {error}")))?;

    read_response(&mut stream, deadline)
}

/// Unix `connect` blocks with no deadline of its own; run it on a helper
/// thread and wait at most `timeout` for the outcome. On timeout the helper
/// thread finishes (and drops the socket) on its own — the caller is free.
#[cfg(unix)]
fn connect_with_deadline(
    socket_path: &str,
    timeout: Duration,
) -> Result<std::os::unix::net::UnixStream> {
    let path = socket_path.to_string();
    let (sender, receiver) = std::sync::mpsc::channel();
    std::thread::Builder::new()
        .name("shift-agent-connect".to_string())
        .spawn(move || {
            let _ = sender.send(
                std::os::unix::net::UnixStream::connect(&path).map_err(|error| error.to_string()),
            );
        })
        .map_err(|error| RequestError::Unreachable(format!("spawn connect thread: {error}")))?;
    match receiver.recv_timeout(timeout) {
        Ok(Ok(stream)) => Ok(stream),
        Ok(Err(error)) => Err(RequestError::Unreachable(format!(
            "cannot connect to agent socket {socket_path}: {error}"
        ))),
        Err(_) => Err(RequestError::Timeout(format!(
            "connect to agent socket {socket_path} exceeded {timeout:?}"
        ))),
    }
}

#[cfg(not(unix))]
pub fn unix_request(
    socket_path: &str,
    _method: &str,
    _path: &str,
    _body: Option<&str>,
) -> Result<HttpResponse> {
    let _ = socket_path;
    Err(RequestError::Protocol(
        "the SHIFT agent is reached over a Unix socket, which this platform does not provide"
            .to_string(),
    ))
}

#[cfg(unix)]
fn read_response(
    stream: &mut std::os::unix::net::UnixStream,
    deadline: Instant,
) -> Result<HttpResponse> {
    let mut buffer: Vec<u8> = Vec::with_capacity(16 * 1024);

    // Read until the end of the header block.
    let header_end = loop {
        if let Some(position) = find_header_end(&buffer) {
            break position;
        }
        if buffer.len() > MAX_HEADER_BYTES {
            return Err(RequestError::Protocol(
                "agent response headers exceed 64 KiB".to_string(),
            ));
        }
        if read_more(stream, &mut buffer, deadline)? == 0 {
            return Err(RequestError::Protocol(
                "agent closed the connection before sending headers".to_string(),
            ));
        }
    };

    let head = String::from_utf8_lossy(&buffer[..header_end]).to_string();
    let (status, headers) = parse_head(&head)?;
    let body_start = header_end + 4;

    let chunked = header_value(&headers, "transfer-encoding")
        .map(is_chunked_framing)
        .unwrap_or(false);

    if chunked {
        // Feed the decoder incrementally: it keeps its own parse cursor, so a
        // large body is decoded once, not re-decoded per 8 KiB read.
        let mut decoder = ChunkedDecoder::new();
        loop {
            if let Some(body) = decoder.push(&buffer[body_start..])? {
                return Ok(HttpResponse { status, body });
            }
            if read_more(stream, &mut buffer, deadline)? == 0 {
                return Err(RequestError::Protocol(
                    "agent closed the connection mid-chunk".to_string(),
                ));
            }
            if buffer.len() - body_start > MAX_BODY_BYTES {
                return Err(RequestError::Protocol(
                    "agent response exceeds 256 MiB".to_string(),
                ));
            }
        }
    }

    if let Some(length) = header_value(&headers, "content-length")
        .and_then(|value| value.trim().parse::<usize>().ok())
    {
        if length > MAX_BODY_BYTES {
            return Err(RequestError::Protocol(
                "agent response exceeds 256 MiB".to_string(),
            ));
        }
        while buffer.len() - body_start < length {
            if read_more(stream, &mut buffer, deadline)? == 0 {
                return Err(RequestError::Protocol(format!(
                    "agent response truncated: {} of {} bytes",
                    buffer.len() - body_start,
                    length
                )));
            }
        }
        return Ok(HttpResponse {
            status,
            body: buffer[body_start..body_start + length].to_vec(),
        });
    }

    // No framing header: the body runs to EOF.
    loop {
        if read_more(stream, &mut buffer, deadline)? == 0 {
            break;
        }
        if buffer.len() - body_start > MAX_BODY_BYTES {
            return Err(RequestError::Protocol(
                "agent response exceeds 256 MiB".to_string(),
            ));
        }
    }
    Ok(HttpResponse {
        status,
        body: buffer[body_start..].to_vec(),
    })
}

/// One bounded read. The read timeout is the time left until the request
/// deadline, so per-read timeouts cannot outlive the total deadline and a
/// slow-dripping response can no longer stretch a request indefinitely.
#[cfg(unix)]
fn read_more(
    stream: &mut std::os::unix::net::UnixStream,
    buffer: &mut Vec<u8>,
    deadline: Instant,
) -> Result<usize> {
    let remaining = deadline.saturating_duration_since(Instant::now());
    if remaining.is_zero() {
        return Err(RequestError::Timeout(
            "agent response exceeded the request deadline".to_string(),
        ));
    }
    let _ = stream.set_read_timeout(Some(remaining));
    let mut chunk = [0u8; 8192];
    match stream.read(&mut chunk) {
        Ok(read) => {
            buffer.extend_from_slice(&chunk[..read]);
            Ok(read)
        }
        Err(error) => match error.kind() {
            std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut => {
                Err(RequestError::Timeout(format!(
                    "agent response exceeded the request deadline ({error})"
                )))
            }
            _ => Err(RequestError::Protocol(format!("read response: {error}"))),
        },
    }
}

/// Locate the `\r\n\r\n` that terminates the header block.
fn find_header_end(buffer: &[u8]) -> Option<usize> {
    buffer.windows(4).position(|window| window == b"\r\n\r\n")
}

fn parse_head(head: &str) -> Result<(u16, Vec<(String, String)>)> {
    let mut lines = head.split("\r\n");
    let status_line = lines
        .next()
        .ok_or(RequestError::Protocol("empty response".to_string()))?;
    let mut parts = status_line.splitn(3, ' ');
    let version = parts.next().unwrap_or_default();
    if !version.starts_with("HTTP/1.") {
        return Err(RequestError::Protocol(format!(
            "unexpected protocol line: {status_line}"
        )));
    }
    let status = parts
        .next()
        .and_then(|code| code.parse::<u16>().ok())
        .ok_or_else(|| RequestError::Protocol(format!("malformed status line: {status_line}")))?;
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

/// Chunked framing is decided by the `chunked` transfer-coding token, matched
/// exactly after comma-splitting — never by a substring that a header like
/// `x-chunked` or `not-chunked` would satisfy.
fn is_chunked_framing(value: &str) -> bool {
    value
        .split(',')
        .map(str::trim)
        .any(|token| token.eq_ignore_ascii_case("chunked"))
}

/// Incremental chunked-body decoder. `push` takes the accumulated body bytes
/// (from the start of the body), resumes from its own parse cursor, and
/// returns the decoded body once the terminal chunk arrives. Chunk sizes are
/// validated before any arithmetic so a hostile size line can never overflow
/// a bounds check into a slice panic.
struct ChunkedDecoder {
    body: Vec<u8>,
    cursor: usize,
    remaining: usize,
    in_chunk_data: bool,
    done: bool,
}

impl ChunkedDecoder {
    fn new() -> Self {
        ChunkedDecoder {
            body: Vec::new(),
            cursor: 0,
            remaining: 0,
            in_chunk_data: false,
            done: false,
        }
    }

    fn push(&mut self, input: &[u8]) -> Result<Option<Vec<u8>>> {
        while !self.done {
            if self.cursor > input.len() {
                return Err(RequestError::Protocol(
                    "chunked decoder cursor out of range".to_string(),
                ));
            }
            if self.in_chunk_data {
                let available = input.len() - self.cursor;
                let take = available.min(self.remaining);
                self.body
                    .extend_from_slice(&input[self.cursor..self.cursor + take]);
                self.cursor += take;
                self.remaining -= take;
                if self.remaining > 0 {
                    return Ok(None);
                }
                if input.len() - self.cursor < 2 {
                    return Ok(None);
                }
                if &input[self.cursor..self.cursor + 2] != b"\r\n" {
                    return Err(RequestError::Protocol(
                        "chunk is not CRLF-terminated".to_string(),
                    ));
                }
                self.cursor += 2;
                self.in_chunk_data = false;
                continue;
            }
            let rest = &input[self.cursor..];
            let line_end = match find_crlf(rest) {
                Some(offset) => self.cursor + offset,
                None => return Ok(None),
            };
            let size_text = String::from_utf8_lossy(&input[self.cursor..line_end]);
            let size_token = size_text.split(';').next().unwrap_or("").trim();
            let size = usize::from_str_radix(size_token, 16).map_err(|_| {
                RequestError::Protocol(format!("malformed chunk size: {size_token:?}"))
            })?;
            if size > MAX_BODY_BYTES {
                return Err(RequestError::Protocol(format!(
                    "chunk of {size} bytes exceeds the 256 MiB body cap"
                )));
            }
            self.cursor = line_end + 2;
            if size == 0 {
                // Terminal chunk — the trailing CRLF (and optional trailers)
                // may still be arriving; the body itself is complete.
                self.done = true;
                break;
            }
            self.remaining = size;
            self.in_chunk_data = true;
        }
        Ok(Some(std::mem::take(&mut self.body)))
    }
}

fn find_crlf(input: &[u8]) -> Option<usize> {
    input.windows(2).position(|window| window == b"\r\n")
}

#[cfg(all(test, unix))]
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
        let directory =
            std::env::temp_dir().join(format!("shift-desktop-test-{}", std::process::id()));
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
        let response = unix_request(
            socket.to_str().unwrap(),
            "POST",
            "/v1/workloads",
            Some("{\"name\":\"a\"}"),
        )
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
        let result =
            unix_request(socket.to_str().unwrap(), "GET", "/v1/workloads", None).expect("request");
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
        let result =
            unix_request(socket.to_str().unwrap(), "GET", "/v1/health", None).expect("request");
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
        let result = unix_request(
            socket.to_str().unwrap(),
            "POST",
            "/v1/checkpoints",
            Some("{}"),
        )
        .expect("request");
        assert_eq!(result.status, 422);
        assert!(result.body.starts_with(b"{\"code\":\"CHECKPOINT_FAILED\""));
        let _ = std::fs::remove_file(&socket);
    }

    #[test]
    fn connection_failure_is_an_error() {
        let result = unix_request("/nonexistent/shift-agent.sock", "GET", "/v1/health", None);
        assert!(result.is_err());
        assert!(result.unwrap_err().to_string().contains("cannot connect"));
    }

    #[test]
    fn oversized_chunk_size_is_a_protocol_error_not_a_panic() {
        // A chunk line of ffffffffffffffff passes parsing but overflows any
        // naive `cursor + size + 2` bounds check. It must surface as a
        // protocol error in every build profile.
        let (socket, _received) = serve_once(
            "chunk-overflow",
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nffffffffffffffff\r\nxx",
        );
        let result = unix_request(socket.to_str().unwrap(), "GET", "/v1/health", None);
        match result {
            Err(RequestError::Protocol(message)) => {
                assert!(message.contains("256 MiB"), "unexpected message: {message}");
            }
            other => panic!("expected a protocol error, got {other:?}"),
        }
        let _ = std::fs::remove_file(&socket);
    }

    #[test]
    fn chunk_size_above_body_cap_is_rejected() {
        let mut decoder = ChunkedDecoder::new();
        let error = decoder
            .push(b"100000001\r\nxx\r\n0\r\n\r\n")
            .expect_err("oversized chunk must be refused");
        assert!(error.to_string().contains("256 MiB"));
    }

    #[test]
    fn malformed_chunk_size_is_a_protocol_error() {
        let mut decoder = ChunkedDecoder::new();
        let error = decoder
            .push(b"zz\r\nxx\r\n")
            .expect_err("non-hex size must be refused");
        assert!(error.to_string().contains("malformed chunk size"));
    }

    #[test]
    fn chunked_decoder_reassembles_across_fragments() {
        // Feed the body one byte at a time: the decoder must resume from its
        // own cursor instead of re-parsing (or losing) earlier bytes.
        let full = b"7\r\n{\"state\r\n7\r\n\":\"ok\"}\r\n0\r\n\r\n";
        let mut decoder = ChunkedDecoder::new();
        let mut accumulated: Vec<u8> = Vec::new();
        let mut decoded = None;
        for byte in full.iter() {
            accumulated.push(*byte);
            decoded = decoder.push(&accumulated).expect("decode");
            if decoded.is_some() {
                break;
            }
        }
        assert_eq!(decoded.expect("terminal chunk"), b"{\"state\":\"ok\"}");
    }

    #[test]
    fn chunked_frame_boundary_is_awaited_not_overrun() {
        // The first chunk's data is complete but its trailing CRLF has not
        // arrived; the decoder must wait, not read past the end.
        let mut decoder = ChunkedDecoder::new();
        assert_eq!(decoder.push(b"3\r\nabc").expect("partial"), None);
        assert_eq!(decoder.push(b"3\r\nabc\r").expect("partial"), None);
        assert_eq!(
            decoder.push(b"3\r\nabc\r\n0\r\n\r\n").expect("complete"),
            Some(b"abc".to_vec())
        );
    }

    #[test]
    fn transfer_coding_requires_the_exact_chunked_token() {
        assert!(is_chunked_framing("chunked"));
        assert!(is_chunked_framing("Chunked"));
        assert!(is_chunked_framing("gzip, chunked"));
        assert!(!is_chunked_framing("x-chunked"));
        assert!(!is_chunked_framing("not-chunked"));
        assert!(!is_chunked_framing("identity"));
    }

    #[test]
    fn non_chunked_te_header_falls_back_to_content_length() {
        // A value merely containing "chunked" must not route into the chunked
        // decoder: framing falls back to Content-Length and the body decodes.
        let (socket, _received) = serve_once(
            "te-not-chunked",
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: not-chunked\r\nContent-Length: 2\r\n\r\nok",
        );
        let result =
            unix_request(socket.to_str().unwrap(), "GET", "/v1/health", None).expect("request");
        assert_eq!(result.body, b"ok");
        let _ = std::fs::remove_file(&socket);
    }

    #[test]
    fn long_routes_get_a_long_deadline_and_ordinary_routes_do_not() {
        assert!(total_deadline("/v1/health") < total_deadline("/v1/migrations"));
        assert!(total_deadline("/v1/workloads") < total_deadline("/v1/checkpoints"));
        assert_eq!(total_deadline("/v1/health"), SHORT_TOTAL);
        assert_eq!(total_deadline("/v1/workloads/w1/logs?tail=10"), LONG_TOTAL);
    }

    #[test]
    fn connect_to_a_dead_socket_fails_fast_with_unreachable() {
        // A socket path that exists but has no listener refuses immediately;
        // a path that does not exist must fail inside the connect deadline.
        let started = std::time::Instant::now();
        let result = unix_request("/nonexistent/shift-agent.sock", "GET", "/v1/health", None);
        assert!(result.is_err());
        assert!(started.elapsed() < CONNECT_TIMEOUT);
    }
}
