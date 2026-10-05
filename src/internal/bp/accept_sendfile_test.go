// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package bp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Files reach the client through respWriter.ReadFrom, which hands them to the
// connection's own ReadFrom (sendfile on plain HTTP/1.1). The status and the
// byte count in the record have to come out as they do when the bytes go
// through Write. A real server uses the ReaderFrom path; the test recorder has
// none, so it covers the fallback.
func TestAcceptFilesBytesOut(t *testing.T) {
	f := newFixture(t, "", false)
	big := bytes.Repeat([]byte("0123456789abcdef"), (3<<20)/16)
	writeFile(t, f.public+"/big.bin", string(big))
	srv := httptest.NewServer(f.h)
	defer srv.Close()

	type result struct {
		status int
		body   []byte
	}
	viaServer := func(method, path, rng string) result {
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "example.com:8080"
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return result{resp.StatusCode, body}
	}
	viaRecorder := func(method, path, rng string) result {
		var hdr []string
		if rng != "" {
			hdr = []string{"Range", rng}
		}
		rr := f.do(method, path, hdr...)
		return result{rr.Code, rr.Body.Bytes()}
	}
	// The record is written when the handler returns, which can be just after
	// the client has the last byte.
	lastRecord := func(n int) Record {
		t.Helper()
		var recs []Record
		for i := 0; i < 200; i++ {
			if recs = f.records(t); len(recs) >= n {
				return recs[n-1]
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%d records, want %d", len(recs), n)
		return Record{}
	}

	n := 0
	for _, via := range []struct {
		name string
		do   func(method, path, rng string) result
	}{{"real server", viaServer}, {"recorder", viaRecorder}} {
		for _, c := range []struct {
			name, method, path, rng string
			status                  int
			want                    []byte
		}{
			{"3 MiB file", "GET", "/big.bin", "", 200, big},
			{"range inside the file", "GET", "/big.bin", "bytes=100-299", 206, big[100:300]},
			{"suffix range", "GET", "/big.bin", "bytes=-1000", 206, big[len(big)-1000:]},
			{"small file", "GET", "/index.html", "", 200, []byte("<h1>home</h1>")},
			{"HEAD of the big file", "HEAD", "/big.bin", "", 200, nil},
			{"range that can't be met", "GET", "/big.bin", "bytes=99999999-", 416, nil},
		} {
			n++
			got := via.do(c.method, c.path, c.rng)
			rec := lastRecord(n)
			if got.status != c.status || rec.Status != c.status {
				t.Errorf("%s, %s: client got %d, record says %d, want %d", via.name, c.name, got.status, rec.Status, c.status)
			}
			if c.want != nil && !bytes.Equal(got.body, c.want) {
				t.Errorf("%s, %s: the client got %d bytes that differ from the file's %d wanted", via.name, c.name, len(got.body), len(c.want))
			}
			if rec.BytesOut != int64(len(got.body)) {
				t.Errorf("%s, %s: record says %d bytes out, the client got %d", via.name, c.name, rec.BytesOut, len(got.body))
			}
			if c.method == "HEAD" && len(got.body) != 0 {
				t.Errorf("%s, %s: HEAD got %d body bytes", via.name, c.name, len(got.body))
			}
		}
	}
}

// accRF is a response writer with a ReadFrom, as the connection's writer has.
type accRF struct {
	*httptest.ResponseRecorder
	calls int
	n     int64
}

func (w *accRF) ReadFrom(r io.Reader) (int64, error) {
	w.calls++
	n, err := io.Copy(struct{ io.Writer }{w.ResponseRecorder}, r)
	w.n += n
	return n, err
}

// A file has to reach the underlying writer's ReadFrom, or sendfile is never
// used; and what goes through it is counted.
func TestAcceptFilesReachReaderFrom(t *testing.T) {
	f := newFixture(t, "", false)
	big := bytes.Repeat([]byte("0123456789abcdef"), (1<<20)/16)
	writeFile(t, f.public+"/big.bin", string(big))
	w := &accRF{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest("GET", "/big.bin", nil)
	req.Host = "example.com:8080"
	f.h.ServeHTTP(w, req)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), big) {
		t.Fatalf("status %d, %d body bytes, want 200 and %d", w.Code, w.Body.Len(), len(big))
	}
	if w.calls == 0 || w.n != int64(len(big)) {
		t.Errorf("the underlying ReadFrom was called %d times for %d bytes, want it to carry all %d", w.calls, w.n, len(big))
	}
	recs := f.records(t)
	if last := recs[len(recs)-1]; last.Status != 200 || last.BytesOut != int64(len(big)) {
		t.Errorf("record says status %d and %d bytes out, want 200 and %d", last.Status, last.BytesOut, len(big))
	}
}
