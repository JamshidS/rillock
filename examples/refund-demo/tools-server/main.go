// Command tools-server is a fake order and payment service for the refund
// demo. It logs every call it receives, so you can watch what the agent does.
//
//	go run ./examples/refund-demo/tools-server
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

var orders = map[string]map[string]any{
	"ord_812": {"order_id": "ord_812", "amount": 300, "status": "delivered", "item": "desk lamp"},
	"ord_900": {"order_id": "ord_900", "amount": 4000, "status": "delivered", "item": "laptop"},
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders/get", handle(func(args map[string]any) (int, any) {
		id, _ := args["order_id"].(string)
		if order, ok := orders[id]; ok {
			return http.StatusOK, order
		}
		return http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no order %q", id)}
	}))
	mux.HandleFunc("POST /payments/refund", handle(func(args map[string]any) (int, any) {
		id, _ := args["order_id"].(string)
		if _, ok := orders[id]; !ok {
			return http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no order %q", id)}
		}
		return http.StatusOK, map[string]any{"refund_id": "re_" + id, "order_id": id, "amount": args["amount"], "status": "refunded"}
	}))

	slog.Info("demo tools listening", "url", "http://"+*addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// handle decodes the JSON arguments, logs the call, and writes the JSON answer.
func handle(fn func(args map[string]any) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&args); err != nil {
			http.Error(w, "invalid JSON arguments", http.StatusBadRequest)
			return
		}
		status, body := fn(args)
		// slog quotes values, so a caller cannot forge log lines with a newline
		// in a header (log injection), as it could with Printf.
		slog.Info("call", "path", r.URL.Path, "args", args, "status", status,
			"idempotencyKey", r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body) // a failed write means the caller left
	}
}
