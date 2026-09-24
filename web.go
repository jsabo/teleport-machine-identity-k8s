package main

import (
	_ "embed"
	"html/template"
	"log"
	"net/http"
)

//go:embed index.html
var indexHTML string

var page = template.Must(template.New("index").Funcs(template.FuncMap{
	"okCount": func(rs []Result) int {
		n := 0
		for _, r := range rs {
			if r.OK {
				n++
			}
		}
		return n
	},
	"healthyCount": func(ss []SidecarService) int {
		n := 0
		for _, s := range ss {
			if s.Status == "healthy" {
				n++
			}
		}
		return n
	},
}).Parse(indexHTML))

func (s *server) page(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	r := s.report(req.Context(), req.URL.Query().Has("refresh"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(w, r); err != nil {
		log.Printf("render: %v", err)
	}
}
