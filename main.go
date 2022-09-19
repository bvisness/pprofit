package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed public/index.html
var index []byte

var (
	pprofitPath  = filepath.Join(must1(os.UserHomeDir()), ".pprofit")
	profilesPath = filepath.Join(pprofitPath, "profiles")
)

type obj map[string]any
type arr []any

type Profile struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	CreatedAt int    `json:"createdAt"`
}

type ProfileType string

var ValidTypes = []ProfileType{"allocs", "block", "cmdline", "goroutine", "heap", "mutex", "profile", "threadcreate", "trace"}

func GetProfileType(name string) string {
	return strings.SplitN(name, "-", 2)[0]
}

var client = http.Client{}

func main() {
	must(os.MkdirAll(pprofitPath, 0750))
	must(os.MkdirAll(profilesPath, 0750))

	hostport := ":"
	if len(os.Args) > 1 {
		hostport = os.Args[1]
	}
	host, port := must2(getHostAndPort(hostport))
	addr := fmt.Sprintf("%s:%d", host, port)

	var backgroundProcesses []*os.Process
	defer func() {
		log.Println("Exiting background processes...")
		for _, p := range backgroundProcesses {
			log.Printf("Killing process %d\n", p.Pid)
			err := p.Kill()
			if err != nil {
				log.Printf("ERROR: failed to kill PID %d, background process may still be running: %v", p.Pid, err)
				continue
			}
		}
	}()

	http.Handle("/", GET(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(index)
	}))
	http.Handle("/profiles", GET(func(w http.ResponseWriter, r *http.Request) {
		files := must1(ioutil.ReadDir(profilesPath))
		profiles := []Profile{}
		for _, f := range files {
			profiles = append(profiles, Profile{
				Name:      f.Name(),
				Type:      GetProfileType(f.Name()),
				CreatedAt: int(f.ModTime().Unix()),
			})
		}
		writeJSON(w, obj{
			"profiles": profiles,
		})
	}))
	http.Handle("/save", POST(func(w http.ResponseWriter, r *http.Request) {
		type SaveRequest struct {
			Url  string `json:"url"`
			Type string `json:"type"`
		}

		var req SaveRequest
		err := readJSON(r, &req)
		if err != nil {
			writeError(w, err, "")
			return
		}

		if req.Url == "" {
			writeError(w, nil, "missing `url`")
			return
		}
		if req.Type == "" {
			writeError(w, nil, "missing `type`")
			return
		}

		res, err := client.Get(req.Url)
		if err != nil {
			writeError(w, err, "failed to get profile")
			return
		}
		defer res.Body.Close()

		outname := fmt.Sprintf("%s-%d", req.Type, time.Now().Unix())
		out := must1(os.Create(filepath.Join(profilesPath, outname)))
		must1(io.Copy(out, res.Body))

		writeJSON(w, Profile{
			Name:      outname,
			Type:      GetProfileType(outname),
			CreatedAt: int(time.Now().Unix()),
		})
	}))
	http.Handle("/open", POST(func(w http.ResponseWriter, r *http.Request) {
		type OpenRequest struct {
			Name string `json:"name"`
		}

		var req OpenRequest
		err := readJSON(r, &req)
		if err != nil {
			writeError(w, err, "")
			return
		}

		if req.Name == "" {
			writeError(w, nil, "missing `name`")
			return
		}

		var cmd *exec.Cmd
		switch GetProfileType(req.Name) {
		case "trace":
			cmd = exec.Command("go", "tool", "trace", filepath.Join(profilesPath, req.Name))
		default:
			cmd = exec.Command("go", "tool", "pprof", "-http=:", filepath.Join(profilesPath, req.Name))
		}

		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		must(cmd.Start())
		log.Printf("Starting pprof for profile %s (PID %d)\n", req.Name, cmd.Process.Pid)

		done := make(chan error)
		go func() {
			done <- cmd.Wait()
		}()

		time.Sleep(2 * time.Second)
		select {
		case err := <-done:
			log.Printf("ERROR: process failed to run: %v", err)
			writeError(w, nil, "process failed to start")
			return
		default:
			log.Printf("Process %d seems to have started up successfully.\n", cmd.Process.Pid)
			backgroundProcesses = append(backgroundProcesses, cmd.Process)
		}

		writeJSON(w, obj{"success": true})
	}))

	srv := http.Server{
		Addr: addr,
	}

	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		<-sigint

		// We received an interrupt signal, shut down.
		if err := srv.Shutdown(context.Background()); err != nil {
			log.Printf("ERROR: failed to shut down server: %v", err)
		}

		// Second signal means force shutdown
		<-sigint
		log.Println("Forcibly shutting down! Background processes may not have been killed.")
		os.Exit(1)
	}()

	log.Println("Listening on", addr)
	go openBrowser(fmt.Sprintf("http://%s/", addr))
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Printf("ERROR: failed to run server: %v", err)
	}
}

func GET(f http.HandlerFunc) http.Handler {
	return Methods([]string{http.MethodGet}, f)
}

func POST(f http.HandlerFunc) http.Handler {
	return Methods([]string{http.MethodPost}, f)
}

func Methods(methods []string, f http.HandlerFunc) http.HandlerFunc {
	return Recover(func(w http.ResponseWriter, r *http.Request) {
		validMethod := false
		for _, method := range methods {
			if r.Method == method {
				validMethod = true
			}
		}
		if !validMethod {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f(w, r)
	})
}

func Recover(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC: %v", r)
				writeError(w, nil, fmt.Sprintf("request panicked: %v", r))
			}
		}()
		f(w, r)
	}
}

// From pprof source
func getHostAndPort(hostport string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", 0, fmt.Errorf("could not split http address: %v", err)
	}
	if host == "" {
		host = "localhost"
	}
	var port int
	if portStr == "" {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return "", 0, fmt.Errorf("could not generate random port: %w", err)
		}
		port = ln.Addr().(*net.TCPAddr).Port
		err = ln.Close()
		if err != nil {
			return "", 0, fmt.Errorf("could not generate random port: %w", err)
		}
	} else {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return "", 0, fmt.Errorf("invalid port number: %w", err)
		}
	}
	return host, port, nil
}

func openBrowser(url string) {
	// Give server a little time to get ready.
	time.Sleep(time.Millisecond * 500)

	for _, b := range browsers() {
		args := strings.Split(b, " ")
		if len(args) == 0 {
			continue
		}
		viewer := exec.Command(args[0], append(args[1:], url)...)
		viewer.Stderr = os.Stderr
		if err := viewer.Start(); err == nil {
			return
		}
	}
	// No visualizer succeeded, so just print URL.
	fmt.Printf("Open %s in your browser.\n", url)
}

func browsers() []string {
	var cmds []string
	if userBrowser := os.Getenv("BROWSER"); userBrowser != "" {
		cmds = append(cmds, userBrowser)
	}
	switch runtime.GOOS {
	case "darwin":
		cmds = append(cmds, "/usr/bin/open")
	case "windows":
		cmds = append(cmds, "cmd /c start")
	default:
		// Commands opening browsers are prioritized over xdg-open, so browser()
		// command can be used on linux to open the .svg file generated by the -web
		// command (the .svg file includes embedded javascript so is best viewed in
		// a browser).
		cmds = append(cmds, []string{"chrome", "google-chrome", "chromium", "firefox", "sensible-browser"}...)
		if os.Getenv("DISPLAY") != "" {
			// xdg-open is only for use in a desktop environment.
			cmds = append(cmds, "xdg-open")
		}
	}
	return cmds
}

// Takes an (error) return and panics if there is an error.
// Helps avoid `if err != nil` in scripts. Use sparingly in real code.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

// Takes a (something, error) return and panics if there is an error.
// Helps avoid `if err != nil` in scripts. Use sparingly in real code.
func must1[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Takes a (something, something, error) return and panics if there is an error.
// Helps avoid `if err != nil` in scripts. Use sparingly in real code.
func must2[T1, T2 any](v1 T1, v2 T2, err error) (T1, T2) {
	if err != nil {
		panic(err)
	}
	return v1, v2
}

type userError struct {
	error
}

func readJSON(r *http.Request, dest any) error {
	if r.Header.Get("Content-Type") != "application/json" {
		return userError{fmt.Errorf("expected Content-Type: application/json, but got %s", r.Header.Get("Content-Type"))}
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	err = json.Unmarshal(bodyBytes, dest)
	if err != nil {
		return userError{err}
	}

	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Add("Content-Type", "application/json")
	must1(w.Write(must1(json.Marshal(v))))
}

func writeError(w http.ResponseWriter, err error, niceMessage string) {
	code := http.StatusInternalServerError
	if _, ok := err.(userError); ok {
		code = http.StatusBadRequest
	}
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(code)

	if err != nil {
		log.Printf("ERROR: %v", err)
	}

	message := niceMessage
	if niceMessage == "" && err != nil {
		message = err.Error()
	}
	must1(w.Write(must1(json.Marshal(obj{
		"error": message,
	}))))
}
