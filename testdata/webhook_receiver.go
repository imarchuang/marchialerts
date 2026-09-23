package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
)

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:9999")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("webhook receiver on http://127.0.0.1:9999/alerts")
	http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		pretty, _ := json.MarshalIndent(p, "", "  ")
		fmt.Printf("--- webhook received ---\n%s\n", pretty)
		w.WriteHeader(http.StatusOK)
	}))
}
