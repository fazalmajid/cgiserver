// code initially generated by Anthropic Claude 3.7 Sonnet
// reworked since by Fazal Majid
//
// Prompts:
//
// Write a Go script that serves HTTP requests and maps them to scripts using
// the CGI interface
//
// How are environment variables sanitized to prevent attacks?
//
// Please implement those security measures
//
// How is the timeout implemented? Will the script be killed if it exceeds the
// timeout?
//
// Yes, please do
//
// Transcript: https://claude.ai/share/eb5f48b8-1794-415c-adc5-7fa5a7a766e3

package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	port              = flag.Int("port", 8080, "Port to listen on")
	cgiDir            = flag.String("cgi-dir", "./cgi-bin", "Directory containing CGI scripts")
	cgiPrefix         = flag.String("cgi-prefix", "/cgi-bin/", "URL prefix for CGI scripts")
	maxEnvSize        = flag.Int("max-env-size", 4096, "Maximum size for environment variables")
	scriptTimeout     = flag.Duration("script-timeout", 30*time.Second, "Timeout for CGI script execution")
	allowedExtensions = flag.String("allowed-extensions", ".cgi", "Comma-separated list of allowed script extensions")
)

// Define a whitelist of allowed HTTP headers to pass to CGI scripts
var allowedHeaders = map[string]bool{
	"ACCEPT":          true,
	"ACCEPT_CHARSET":  true,
	"ACCEPT_ENCODING": true,
	"ACCEPT_LANGUAGE": true,
	"AUTHORIZATION":   true,
	"CONTENT_LENGTH":  true,
	"CONTENT_TYPE":    true,
	"COOKIE":          true,
	"HOST":            true,
	"REFERER":         true,
	"USER_AGENT":      true,
	"X_FORWARDED_FOR": true,
}

func main() {
	flag.Parse()

	// Create CGI handler
	cgiHandler := http.StripPrefix(*cgiPrefix, http.HandlerFunc(handleCGI))

	// Setup routing
	http.Handle(*cgiPrefix, cgiHandler)

	// Start server
	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Starting secure CGI server on http://localhost%s", addr)
	log.Printf("CGI scripts directory: %s", *cgiDir)
	log.Printf("CGI URL prefix: %s", *cgiPrefix)
	log.Printf("Script timeout: %s", *scriptTimeout)

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func handleCGI(w http.ResponseWriter, r *http.Request) {
	// Validate the path to prevent directory traversal
	if !isPathSafe(r.URL.Path) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		log.Printf("Rejected unsafe path: %s", r.URL.Path)
		return
	}

	// Extract script path from request
	scriptPath := filepath.Join(*cgiDir, r.URL.Path)

	// Ensure the script doesn't escape the CGI directory
	absScriptPath, err := filepath.Abs(scriptPath)
	absCGIDir, err2 := filepath.Abs(*cgiDir)

	if err != nil || err2 != nil || !strings.HasPrefix(absScriptPath, absCGIDir) {
		http.Error(w, "Invalid script path", http.StatusForbidden)
		log.Printf("Directory traversal attempt detected: %s", scriptPath)
		return
	}

	// Check file extension against whitelist
	if !hasAllowedExtension(scriptPath) {
		http.Error(w, "Script type not allowed", http.StatusForbidden)
		log.Printf("Rejected script with disallowed extension: %s", scriptPath)
		return
	}

	// Check if file exists and is executable
	info, err := os.Stat(scriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Script not found", http.StatusNotFound)
		} else {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			log.Printf("Error accessing script %s: %v", scriptPath, err)
		}
		return
	}

	// Check if it's a regular file
	if !info.Mode().IsRegular() {
		http.Error(w, "Not a valid script", http.StatusForbidden)
		return
	}

	// Check if it's executable (on Unix systems)
	if info.Mode()&0111 == 0 {
		http.Error(w, "Script is not executable", http.StatusForbidden)
		log.Printf("Warning: Script %s is not executable", scriptPath)
		return
	}

	// Create a custom environment for the CGI script with sanitized variables
	env, err := createSanitizedEnvironment(r)
	if err != nil {
		http.Error(w, "Invalid request data", http.StatusBadRequest)
		log.Printf("Environment sanitization error: %v", err)
		return
	}

	// Create a context with timeout for script execution
	ctx, cancel := context.WithTimeout(r.Context(), *scriptTimeout)
	defer cancel()

	// Execute the CGI script with our own implementation that enforces timeouts
	if err := executeCGIWithTimeout(ctx, w, r, scriptPath, env); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			http.Error(w, "Script execution timed out", http.StatusGatewayTimeout)
			log.Printf("Script timed out after %s: %s", *scriptTimeout, scriptPath)
		} else {
			http.Error(w, "Error executing script", http.StatusInternalServerError)
			log.Printf("Error executing script %s: %v", scriptPath, err)
		}
	}
}

// executeCGIWithTimeout runs a CGI script with a hard timeout
func executeCGIWithTimeout(ctx context.Context, w http.ResponseWriter, r *http.Request, scriptPath string, env []string) error {
	// Determine the interpreter based on file extension
	args := []string{}

	// bypass exec.LookPath() and force using the executable in the cgi-bin dir
	executable := "./" + filepath.Base(scriptPath)
	// Create the command with the provided environment
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = env
	cmd.Dir = filepath.Dir(scriptPath)

	// Set up process group for easier termination
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // Create a new process group
	}

	// Set up pipes for stdin, stdout, stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start script: %v", err)
	}

	// Store the process ID for potential forceful termination
	pid := cmd.Process.Pid
	pgid, _ := syscall.Getpgid(pid)

	// Set up a goroutine to handle forceful termination on timeout
	go func() {
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("Force killing process group %d (PID %d)", pgid, pid)
			// Send SIGKILL to the entire process group
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}()

	// Copy request body to script's stdin if needed
	if r.Body != nil {
		_, err := io.Copy(stdin, r.Body)
		if err != nil {
			log.Printf("Error copying request body: %v", err)
		}
	}
	stdin.Close()

	// Process script output
	go func() {
		// Read stderr and log it
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("CGI stderr: %s", scanner.Text())
		}
	}()

	// Parse CGI response
	return parseCGIResponse(stdout, w)
}

// parseCGIResponse processes the CGI script's output and sends it to the client
func parseCGIResponse(stdout io.Reader, w http.ResponseWriter) error {
	// Read the complete output
	var output bytes.Buffer
	_, err := io.Copy(&output, stdout)
	if err != nil {
		return fmt.Errorf("error reading script output: %v", err)
	}

	// Reset to read from the beginning
	data := output.Bytes()
	reader := bufio.NewReader(bytes.NewReader(data))

	// Parse headers
	headers := make(map[string]string)
	statusCode := 200

	for {
		line, err := reader.ReadString('\n')
		if err != nil || line == "\r\n" || line == "\n" {
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Handle special Status header
		if strings.EqualFold(key, "Status") {
			statusParts := strings.SplitN(value, " ", 2)
			if len(statusParts) > 0 {
				if code, err := strconv.Atoi(statusParts[0]); err == nil {
					statusCode = code
				}
			}
		} else {
			headers[key] = value
		}
	}

	// Find the body start position
	bodyStart := bytes.Index(data, []byte("\r\n\r\n"))
	if bodyStart == -1 {
		bodyStart = bytes.Index(data, []byte("\n\n"))
		if bodyStart == -1 {
			// No header separator found, assume all content is body
			bodyStart = 0
		} else {
			bodyStart += 2
		}
	} else {
		bodyStart += 4
	}

	// Set response status
	w.WriteHeader(statusCode)

	// Set response headers
	for key, value := range headers {
		if !strings.EqualFold(key, "Status") {
			w.Header().Set(key, value)
		}
	}

	// Write the body
	_, err = w.Write(data[bodyStart:])
	return err
}

// isPathSafe checks if a path is safe (no directory traversal)
func isPathSafe(p string) bool {
	// see: https://dzx.cz/2021-04-02/go_path_traversal/
	clean := path.Join("/", p)
	return "/"+p == clean
}

// hasAllowedExtension checks if file has a permitted extension
func hasAllowedExtension(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	allowed := strings.Split(*allowedExtensions, ",")

	for _, allowedExt := range allowed {
		if ext == strings.TrimSpace(allowedExt) {
			return true
		}
	}

	return false
}

// createSanitizedEnvironment builds a safe environment for CGI scripts
func createSanitizedEnvironment(r *http.Request) ([]string, error) {
	env := []string{
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_SOFTWARE=Go-CGI-Server/1.0",
	}

	// Add basic CGI variables with sanitization
	clientIp := r.Header.Get("X-Forwarded-For")
	if clientIp == "" {
		clientIp = r.RemoteAddr
	}
	cgiVars := map[string]string{
		"SERVER_NAME":     r.Host,
		"SERVER_PROTOCOL": r.Proto,
		"SERVER_PORT":     r.URL.Port(),
		"REQUEST_METHOD":  r.Method,
		"PATH_INFO":       r.URL.Path,
		"SCRIPT_NAME":     *cgiPrefix + r.URL.Path,
		"QUERY_STRING":    r.URL.RawQuery,
		"REMOTE_ADDR":     clientIp,
		"CONTENT_LENGTH":  r.Header.Get("Content-Length"),
		"CONTENT_TYPE":    r.Header.Get("Content-Type"),
	}

	for name, value := range cgiVars {
		// Check size limit
		if len(value) > *maxEnvSize {
			return nil, fmt.Errorf("environment variable %v exceeds maximum allowed size %v", name, *maxEnvSize)
		}

		var sanitized string
		var err error
		if name == "QUERY_STRING" {
			sanitized, err = value, nil
		} else {
			sanitized, err = sanitizeEnv(value)
		}
		if err != nil {
			return nil, err
		}
		env = append(env, fmt.Sprintf("%s=%s", name, sanitized))
	}

	// Add sanitized HTTP headers as CGI variables
	for header, values := range r.Header {
		headerName := strings.ToUpper(strings.Replace(header, "-", "_", -1))

		// Skip headers not in the whitelist
		if !allowedHeaders[headerName] {
			continue
		}

		for _, value := range values {
			sanitized, err := sanitizeEnv(value)
			if err != nil {
				return nil, err
			}
			env = append(env, fmt.Sprintf("HTTP_%s=%s", headerName, sanitized))
		}
	}

	return env, nil
}

// sanitizeEnv removes potentially dangerous characters from environment variables
// and enforces size limits
func sanitizeEnv(input string) (string, error) {
	// Remove NULL bytes and other control characters
	result := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1 // drop the character
		}
		return r
	}, input)

	// Remove potentially dangerous shell metacharacters
	result = strings.Map(func(r rune) rune {
		// Common shell metacharacters: ; & | ` $ > < ! ( ) { } [ ] \ ^
		if strings.ContainsRune(";|&`$><()[]{}^!\"\\", r) {
			return ' ' // replace with space
		}
		return r
	}, result)

	return result, nil
}
