<div align="center">

# racearmor

**HTTP keepalive race condition sentry and zero-code 502 eliminator.**

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](go.mod)
[![Zero Dependencies](https://img.shields.io/badge/Dependencies-Zero%20(Pure%20Go)-green)](#)

<p align="center">
  <a href="#the-problem">The problem</a> •
  <a href="#how-it-works">How it works</a> •
  <a href="#architecture">Architecture</a> •
  <a href="#comparison">Comparison</a> •
  <a href="#installation">Installation</a> •
  <a href="#usage">Usage</a> •
  <a href="#license">License</a>
</p>

</div>

## The problem

Modern web deployments place reverse proxies or cloud load balancers in front of application backends. To reduce handshake overhead, proxies reuse persistent TCP connections across multiple client requests.

A race condition occurs when the backend application tears down an idle connection at the exact moment the upstream load balancer sends a new incoming request down that connection:

```
  Client             AWS ALB / Cloudflare              Backend (Node/Python)
    |                         |                                  |
    |                         |  (Idle for 5 seconds...)         |
    |                         |                                  | [Backend timer hits 5s]
    |                         |                                  | TCP FIN sent
    |  GET /api/checkout      |                                  |      \
    |------------------------>|                                  |       \ Race condition
    |                         |  GET /api/checkout (reusing)     |       / in transit
    |                         |--------------------------------->|      /
    |                         |                                  | <---'
    |                         |                                  | Kernel sends TCP RST
    |                         |  <-------------------------------|
    |  HTTP 502 Bad Gateway   |
    |<------------------------|
```

Because the proxy has already forwarded bytes over the connection, many load balancers classify the abrupt TCP RST as an unrecoverable failure and immediately return `HTTP 502 Bad Gateway` to the caller.

This occurs because common deployment defaults violate the **Inverted Timeout Hierarchy**:
- **AWS ALB**: 60s idle timeout
- **Cloudflare**: 100s idle timeout
- **Nginx Ingress**: 65s idle timeout
- **Node.js (`http.Server`)**: 5s default `keepAliveTimeout`
- **Python Gunicorn**: 2s default `--keep-alive`
- **Python Uvicorn**: 5s default `--timeout-keep-alive`
- **Ruby Puma**: 20s default `persistent_timeout`

Whenever backend idle timeout is lower than upstream proxy idle timeout, sporadic 502 errors appear during low-to-moderate traffic transitions.

## How it works

`racearmor` inspects keepalive disconnect timing, validates timeout hierarchies, and transparently absorbs socket drops before they reach clients:

1. **`doctor`**: Establishes a persistent HTTP connection to a running backend, waits for the socket to close, and measures the exact idle timeout with microsecond precision. It then calculates the collision window against your upstream proxy tier.
2. **`audit`**: Evaluates static or configured runtime values against target load balancers and produces copy-paste remediation patches for Node.js, Python, Ruby, and Go.
3. **`shield`**: A zero-code loopback reverse proxy sidecar. It buffers in-flight request headers and bodies. If a backend drops the connection before returning HTTP response headers, `shield` immediately re-dials a fresh connection and replays the request. Upstream proxies never see a dropped connection or synthetic 502.

```
  Upstream Proxy                   racearmor shield                Application Backend
       |                                  |                                 |
       |  GET /api/data                   |                                 |
       |--------------------------------->|                                 |
       |                                  |  [Buffers request]              |
       |                                  |  GET /api/data (stale socket)   |
       |                                  |-------------------------------->|
       |                                  |                                 | [Socket closed / RST]
       |                                  |  < - - - - - - - - - - - - - - -|
       |                                  |  [Interception: race detected]  |
       |                                  |  [Opens fresh TCP socket]       |
       |                                  |  GET /api/data (fresh socket)   |
       |                                  |-------------------------------->|
       |                                  |  HTTP 200 OK                    |
       |                                  |  <------------------------------|
       |  HTTP 200 OK (X-RaceArmor-Recovered)                               |
       |<---------------------------------|                                 |
```

## Architecture

`racearmor` is written in pure Go with zero external dependencies.

```
racearmor/
├── cmd/
│   └── racearmor/
│       ├── main.go             # CLI interface: doctor, audit, shield
│       └── main_test.go        # Command line integration tests
├── pkg/
│   ├── prober/
│   │   ├── prober.go           # Precision TCP socket disconnect timer
│   │   └── prober_test.go      # Measurement tests using mock servers
│   ├── audit/
│   │   ├── audit.go            # Hierarchy rules and race window math
│   │   ├── recommendations.go  # Code patch generators for major runtimes
│   │   └── audit_test.go       # Audit and patch verification tests
│   └── shield/
│       ├── absorber.go         # Transparent loopback sidecar with replay
│       └── absorber_test.go    # 502 absorption and retry tests
├── go.mod
└── README.md
```

## Installation

Building from source:

```bash
git clone https://github.com/OGAbhinavMaurya/racearmor.git
cd racearmor
go build -o racearmor ./cmd/racearmor
sudo install -m 0755 racearmor /usr/local/bin/
```

## Usage

### 1. Doctor (active probe)

Measure live backend disconnect timing against your upstream proxy tier:

```bash
# Probe local application against default AWS ALB timeout (60s)
racearmor doctor --target http://127.0.0.1:3000 --proxy alb

# Probe against Cloudflare (100s) with 110s probe timeout
racearmor doctor --target http://127.0.0.1:8000 --proxy cloudflare --max-wait 110s

# Machine-readable JSON output
racearmor doctor --target http://127.0.0.1:3000 --proxy alb --json
```

Example doctor output:
```text
================================================================================
                            RACEARMOR DOCTOR REPORT                             
================================================================================
 Target URL:             http://127.0.0.1:3000
 HTTP Status Code:       200
 Connection Header:      keep-alive
 Measured Backend Idle:  5.002s
 Upstream Proxy Tier:    alb (1m0s)
--------------------------------------------------------------------------------
 Status:                 [CRITICAL]
 Race Window:            54.998s
 Recommended Backend:    >= 1m5s
 Summary:                Critical 502 race window (54.998s). Proxy idle timeout (1m0s) exceeds backend (5.002s). The backend drops idle sockets before the proxy retires them.
--------------------------------------------------------------------------------
 Remediation:
// Set keepalive timeouts greater than the upstream proxy (65s)
const server = app.listen(port, () => {
  console.log("Listening on port " + port);
});

server.keepAliveTimeout = 65000; // 65 seconds
server.headersTimeout = 66000;   // 66 seconds

================================================================================
```

### 2. Audit (configuration calculator)

Audit your static runtime timeout settings against common ingress proxies:

```bash
# Audit Node.js default against AWS ALB
racearmor audit --runtime nodejs --proxy alb

# Audit custom Python Gunicorn setting against Cloudflare
racearmor audit --runtime gunicorn --backend-timeout 65s --proxy cloudflare
```

### 3. Shield (zero-code sidecar)

Deploy `racearmor shield` as a local sidecar or wrapper in front of legacy services, third-party containers, or microservices:

```bash
# Listen on 8080 and forward to application on 3000
racearmor shield --listen :8080 --upstream http://127.0.0.1:3000 --retries 2
```

Whenever the upstream backend drops an idle socket, `shield` transparently catches the failure, opens a new TCP connection, replays the buffered request, and appends the `X-RaceArmor-Recovered` diagnostic header.

## Comparison

| Feature | `racearmor` | Standard reverse proxy | Application retries |
| :--- | :--- | :--- | :--- |
| **Live disconnect prober** | Yes (microsecond precision) | No | No |
| **Hierarchy risk calculator** | Yes (CLI + JSON) | No | No |
| **Code remediation generator** | Yes (Node, Python, Ruby, Go) | No | No |
| **Pre-header socket recovery** | Yes (automatic sidecar) | No (returns 502 on RST) | Partial (risks double-POST) |
| **Zero code changes required** | Yes | No | No |
| **Runtime footprint** | Single static Go binary (~8MB) | Heavy (Nginx/Envoy config) | Requires code rewrites |

## License

MIT
