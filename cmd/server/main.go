// Command server 启动灵问问答平台：HTTP API、Gitea Webhook 接收与后台任务。
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("PORTAL_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	log.Printf("lingo listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
