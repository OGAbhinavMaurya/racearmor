package audit

import (
	"fmt"
	"math"
	"time"
)

// GeneratePatch returns human-readable instructions and an exact code/configuration patch
// for the given backend runtime to ensure its keep-alive timeout satisfies the proxy requirements.
func GeneratePatch(runtime string, targetTimeout time.Duration) (string, string) {
	norm := NormalizeTier(runtime)
	seconds := int(math.Ceil(targetTimeout.Seconds()))
	if seconds <= 0 {
		seconds = 65
	}
	millis := seconds * 1000

	switch norm {
	case "node", "nodejs":
		instructions := fmt.Sprintf("In your Node.js HTTP/Express entrypoint, configure keepAliveTimeout to %dms and headersTimeout to %dms (headersTimeout must exceed keepAliveTimeout).", millis, millis+1000)
		code := fmt.Sprintf(`// Set keepalive timeouts greater than the upstream proxy (%ds)
const server = app.listen(port, () => {
  console.log("Listening on port " + port);
});

server.keepAliveTimeout = %d; // %d seconds
server.headersTimeout = %d;   // %d seconds
`, seconds, millis, seconds, millis+1000, seconds+1)
		return instructions, code

	case "gunicorn", "python-gunicorn":
		instructions := fmt.Sprintf("Pass --keep-alive %d to the gunicorn command line or define keepalive = %d in gunicorn.conf.py.", seconds, seconds)
		code := fmt.Sprintf(`# CLI flag:
gunicorn --keep-alive %d --workers 4 app:wsgi_app

# Or in gunicorn.conf.py:
keepalive = %d
`, seconds, seconds)
		return instructions, code

	case "uvicorn", "python-uvicorn":
		instructions := fmt.Sprintf("Pass --timeout-keep-alive %d to uvicorn CLI or configure timeout_keep_alive in Uvicorn Config.", seconds)
		code := fmt.Sprintf(`# CLI flag:
uvicorn app:main --host 0.0.0.0 --port 8000 --timeout-keep-alive %d

# Or in Python:
import uvicorn
uvicorn.run("app:main", host="0.0.0.0", port=8000, timeout_keep_alive=%d)
`, seconds, seconds)
		return instructions, code

	case "puma", "ruby-puma":
		instructions := fmt.Sprintf("Set persistent_timeout %d in your config/puma.rb file.", seconds)
		code := fmt.Sprintf(`# in config/puma.rb:
persistent_timeout %d
`, seconds)
		return instructions, code

	case "go", "golang", "go-http":
		instructions := fmt.Sprintf("Configure IdleTimeout on your http.Server instance to %d * time.Second.", seconds)
		code := fmt.Sprintf(`// Configure http.Server IdleTimeout
server := &http.Server{
    Addr:         ":8080",
    Handler:      mux,
    IdleTimeout:  %d * time.Second,
    ReadTimeout:  15 * time.Second,
    WriteTimeout: 15 * time.Second,
}
log.Fatal(server.ListenAndServe())
`, seconds)
		return instructions, code

	default:
		instructions := fmt.Sprintf("Configure your backend server keep-alive idle timeout to at least %d seconds.", seconds)
		code := fmt.Sprintf(`# Keepalive configuration:
keepalive_timeout = %d  # seconds
`, seconds)
		return instructions, code
	}
}
