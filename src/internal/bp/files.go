// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// FileResult is what a files rule decides for a path, before anything is read.
type FileResult struct {
	Status   int      // 200, 301 or 404; 0 when there is no folder to look in
	Rel      string   // the file inside the folder
	Checked  []string // what was looked for, inside the folder
	Reason   string   // why a 404
	Location string   // where a 301 points
}

// LookupFile applies the files rules to a normal-form path:
//   - /about/ serves about/index.html, and / serves index.html
//   - /about serves the file about, or redirects to /about/ if it's a folder
//   - folders are never listed, and hidden names (.git, .env) are never
//     served, except /.well-known/
//
// Every lookup goes through os.Root, so nothing outside the folder is
// reached, symlinks included. No path handed to os.Root ends in a slash:
// a folder request asks for its index.html instead.
func LookupFile(root *os.Root, norm string) FileResult {
	dirReq := strings.HasSuffix(norm, "/")
	var names []string
	for i, s := range strings.Split(strings.Trim(norm, "/"), "/") {
		if s == "" {
			continue
		}
		d, err := url.PathUnescape(s)
		if err != nil || d == "" || strings.ContainsAny(d, "/\\\x00") {
			return FileResult{Status: 404, Reason: "this path can't name a file"}
		}
		if strings.HasPrefix(d, ".") && !(i == 0 && d == ".well-known") {
			return FileResult{Status: 404, Reason: "hidden name " + d + " is never served"}
		}
		names = append(names, d)
	}
	rel := strings.Join(names, "/")
	if dirReq {
		idx := path.Join(rel, "index.html")
		res := FileResult{Checked: []string{idx}}
		if root == nil {
			return res // no folder is open (browser demo): Status stays 0
		}
		fi, err := root.Stat(idx)
		if err != nil || !fi.Mode().IsRegular() {
			res.Status, res.Reason = 404, statReason(fi, err)
			return res
		}
		res.Status, res.Rel = 200, idx
		return res
	}
	res := FileResult{Checked: []string{rel}}
	if root == nil {
		return res
	}
	fi, err := root.Stat(rel)
	switch {
	case err != nil:
		res.Status, res.Reason = 404, statReason(fi, err)
	case fi.IsDir():
		res.Status, res.Location = 301, norm+"/"
	case !fi.Mode().IsRegular():
		res.Status, res.Reason = 404, "not a regular file"
	default:
		res.Status, res.Rel = 200, rel
	}
	return res
}

func statReason(fi fs.FileInfo, err error) string {
	switch {
	case err == nil && fi != nil && fi.IsDir():
		return "it's a folder"
	case err == nil:
		return "not a regular file"
	case errors.Is(err, fs.ErrNotExist):
		return "no such file"
	case strings.Contains(err.Error(), "escapes"):
		return "it leads outside the site folder"
	case errors.Is(err, fs.ErrPermission):
		return "no permission to read it"
	}
	return unwrapPathErr(err).Error()
}

func unwrapPathErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func inFolder(dir, rel string) string {
	return filepath.Join(dir, filepath.FromSlash(rel))
}

var webTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8",
	".mjs": "text/javascript; charset=utf-8", ".json": "application/json",
	".map": "application/json", ".xml": "application/xml",
	".txt": "text/plain; charset=utf-8", ".md": "text/markdown; charset=utf-8",
	".csv": "text/csv; charset=utf-8", ".svg": "image/svg+xml",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".avif": "image/avif",
	".ico": "image/x-icon", ".woff": "font/woff", ".woff2": "font/woff2",
	".ttf": "font/ttf", ".otf": "font/otf", ".pdf": "application/pdf",
	".wasm": "application/wasm", ".mp4": "video/mp4", ".webm": "video/webm",
	".mp3": "audio/mpeg", ".webmanifest": "application/manifest+json",
	".zip": "application/zip", ".gz": "application/gzip",
}

// ContentType picks a file's type from its name alone, so explain can say
// what a request would get without reading the file.
func ContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := webTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// Rep is the file chosen to answer a request: the file itself, or a
// precompressed copy of it.
type Rep struct {
	Name     string
	Encoding string
}

// ChooseRep picks a precompressed copy, .br before .gz, when the client
// accepts it. It also returns the copies that exist.
func ChooseRep(root *os.Root, rel, acceptEncoding string) (Rep, []string) {
	var variants []string
	br := isRegular(root, rel+".br")
	gz := isRegular(root, rel+".gz")
	if br {
		variants = append(variants, rel+".br")
	}
	if gz {
		variants = append(variants, rel+".gz")
	}
	switch {
	case br && accepts(acceptEncoding, "br"):
		return Rep{rel + ".br", "br"}, variants
	case gz && accepts(acceptEncoding, "gzip"):
		return Rep{rel + ".gz", "gzip"}, variants
	}
	return Rep{Name: rel}, variants
}

func isRegular(root *os.Root, name string) bool {
	fi, err := root.Stat(name)
	return err == nil && fi.Mode().IsRegular()
}

// accepts reports whether an Accept-Encoding header allows a coding.
func accepts(h, coding string) bool {
	star := false
	for _, part := range strings.Split(h, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		q := 1.0
		for _, prm := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(prm), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		if name == coding {
			return q > 0
		}
		if name == "*" && q > 0 {
			star = true
		}
	}
	return star
}

func etag(fi fs.FileInfo, enc string) string {
	suffix := ""
	switch enc {
	case "br":
		suffix = "-br"
	case "gzip":
		suffix = "-gz"
	}
	return fmt.Sprintf(`"%x-%x%s"`, fi.ModTime().UnixNano(), fi.Size(), suffix)
}

// serveFile answers with one file. http.ServeContent handles HEAD, byte
// ranges and conditional requests; BareProxy sets the type, ETag and
// encoding first.
func serveFile(w http.ResponseWriter, r *http.Request, root *os.Root, rep Rep, ctype string, vary bool) error {
	f, err := root.Open(rep.Name)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	if rep.Encoding != "" {
		h.Set("Content-Encoding", rep.Encoding)
	}
	if vary {
		h.Add("Vary", "Accept-Encoding")
	}
	h.Set("ETag", etag(fi, rep.Encoding))
	if strings.HasPrefix(ctype, "text/html") {
		h.Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, "", fi.ModTime(), f)
	return nil
}

// serveErrorPage sends a file as the body of an error response.
func serveErrorPage(w http.ResponseWriter, r *http.Request, root *os.Root, rel string, status int) error {
	f, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", ContentType(rel))
	h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		io.Copy(w, f)
	}
	return nil
}

// ErrorPage finds a site's 404 page by routing its path like a request, so
// it comes from whichever files rule would serve that path.
func (s *Site) ErrorPage() (*Route, FileResult, bool) {
	if s.Err404 == "" {
		return nil, FileResult{}, false
	}
	rt, _ := s.match(http.MethodGet, s.Err404, nil, false)
	if rt == nil || rt.Act.Kind != "files" {
		return rt, FileResult{}, false
	}
	fr := LookupFile(rt.Act.Root, s.Err404)
	return rt, fr, fr.Status == 200
}
