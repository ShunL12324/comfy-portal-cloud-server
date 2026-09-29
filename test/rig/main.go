// Command rig is the test rig's stand-in services, so the end-to-end test
// needs no Python, no network and no GPU.
//
//	rig comfy --port N               answers /system_stats like ComfyUI
//	rig fixtures --dir D --port N    serves D over loopback
//	rig fixtures --make name=MiB ... create random files in --dir first
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rig comfy|fixtures ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "comfy":
		comfy(os.Args[2:])
	case "fixtures":
		fixtures(os.Args[2:])
	default:
		os.Exit(2)
	}
}

// comfy accepts ComfyUI's real argv (main.py --listen 0.0.0.0 --port N) so the
// supervisor's command line works unchanged through a wrapper script.
func comfy(args []string) {
	port := "8188"
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			port = args[i+1]
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"system":{"comfyui_version":"fake"}}`)
	})
	fatal(http.ListenAndServe("127.0.0.1:"+port, mux))
}

func fixtures(args []string) {
	fs := flag.NewFlagSet("fixtures", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory to serve")
	port := fs.String("port", "8000", "port")
	_ = fs.Parse(args)
	for _, spec := range fs.Args() { // name=MiB
		name, mib, ok := strings.Cut(spec, "=")
		n, err := strconv.Atoi(mib)
		if !ok || err != nil {
			fatal(fmt.Errorf("bad fixture %q", spec))
		}
		f, err := os.Create(filepath.Join(*dir, name))
		fatal(err)
		_, err = io.CopyN(f, rand.Reader, int64(n)<<20)
		fatal(err)
		fatal(f.Close())
	}
	fatal(http.ListenAndServe("127.0.0.1:"+*port, http.FileServer(http.Dir(*dir))))
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "rig:", err)
		os.Exit(1)
	}
}
