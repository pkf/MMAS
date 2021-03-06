package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/rakoo/mmas/pkg/dict"
)

type SDCHProxy struct {
	proxy  *httputil.ReverseProxy
	d      *dict.Dict
	target *url.URL
}

func newSDCHProxy(target *url.URL) SDCHProxy {
	iproxy := httputil.NewSingleHostReverseProxy(target)
	pDirector := iproxy.Director
	iproxy.Director = func(r *http.Request) {
		pDirector(r)
		r.Host = r.URL.Host
	}

	d, err := dict.New()
	if err != nil {
		log.Fatal(err)
	}
	return SDCHProxy{
		proxy:  iproxy,
		d:      d,
		target: target,
	}
}

func (s SDCHProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_sdch") {
		s.serveDict(w, r)
		return
	}

	canSdch := false
	w.Header().Set("X-Sdch-Encode", "0")

	if len(s.d.SdchHeader) > 0 {
		path := fmt.Sprintf("/_sdch/%s", s.d.DictName())
		w.Header().Set("Get-Dictionary", path)
	}

	aes := r.Header["Accept-Encoding"]
	for _, ae := range aes {
		split := strings.Split(ae, ",")
		for _, each := range split {
			if strings.TrimSpace(each) == "sdch" {
				canSdch = true
			}
		}
	}

	if !canSdch {
		s.proxy.ServeHTTP(w, r)
		return
	}

	rr := httptest.NewRecorder()
	s.proxy.ServeHTTP(rr, r)
	copyHeader(w.Header(), rr.Header())

	isTextHtml := false
	cts := rr.Header()["Content-Type"]
	for _, ct := range cts {
		if strings.HasPrefix(ct, "text/html") {
			isTextHtml = true
		}
	}

	if !isTextHtml {
		io.Copy(w, rr.Body)
		return
	}

	// Read content, ungzip it if needed
	originalContent := rr.Body.Bytes()
	workContent := originalContent

	ces := rr.Header()["Content-Encoding"]
	hasGzip := false
	for _, ce := range ces {
		if ce == "gzip" {
			hasGzip = true
			break
		}
	}
	if hasGzip {
		gzr, err := gzip.NewReader(rr.Body)
		if err != nil {
			httpError(w)
			return
		}
		workContent, err = ioutil.ReadAll(gzr)
		if err != nil {
			httpError(w)
			return
		}
	}

	diff, err := s.d.Eat(workContent)
	if err != nil {
		if err != dict.ErrNoDict {
			log.Println("Error eating:", err)
		}
		// If all else fails, return original response
		w.Write(originalContent)
		return
	}

	newContent := originalContent
	if hasGzip {
		var buf bytes.Buffer
		gzw := gzip.NewWriter(&buf)
		gzw.Write(diff)
		gzw.Flush()
		newContent = buf.Bytes()
	}

	ratio := 100 * float64(len(newContent)) / float64(len(originalContent))
	log.Printf("Ratio: %d/%d (%f%%)", len(newContent), len(originalContent), ratio)

	if len(newContent) > len(originalContent) {
		w.Write(originalContent)
		return
	}

	// Like Chrome, we only take the first one
	uaId := r.Header.Get("Avail-Dictionary")
	if len(uaId) == 0 || uaId != string(s.d.UserAgentId()) {
		log.Printf("UA wants %s, we have %s\n", uaId, s.d.UserAgentId())
		w.Write(originalContent)
		return
	}

	w.Header().Set("Content-Encoding", "sdch")
	if hasGzip {
		w.Header().Add("Content-Encoding", "gzip")
	}
	w.Header().Del("X-Sdch-Encode")

	serverId := s.d.ServerId()
	cl := strconv.Itoa(len(serverId) + 1 + len(newContent))
	w.Header().Set("Content-Length", cl)
	w.Write(serverId)
	w.Write([]byte{0})
	w.Write(newContent)
}

func (s SDCHProxy) serveDict(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	_, err := buf.Write(s.d.SdchHeader)
	if err != nil {
		httpError(w)
		return
	}

	name := strings.Replace(r.URL.Path, "/_sdch/", "", 1)
	f, err := os.Open(path.Join("dicts", name))
	if err != nil {
		httpError(w)
		return
	}

	_, err = io.Copy(&buf, f)
	if err != nil {
		httpError(w)
		return
	}

	st, err := f.Stat()
	if err != nil {
		httpError(w)
		return
	}

	w.Header().Set("Content-Type", "application/x-sdch-dictionary")
	http.ServeContent(w, r, "", st.ModTime(), bytes.NewReader(buf.Bytes()))
}

// Same as httputil/reverseproxy.go
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func httpError(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

func main() {
	u, err := url.Parse("https://en.wikipedia.org/")
	if err != nil {
		log.Fatal(err)
	}
	proxy := newSDCHProxy(u)

	log.Println("Let's go !")
	log.Fatal(http.ListenAndServe(":8080", proxy))
}
