# Refund demo

An agent that looks up an order and refunds it, running entirely offline.
The model is the `scripted` fake: the run input says what it should reply at
each turn, so you control exactly what "the model decides".

## Run it

You need three terminals. See [running locally](../../docs/local-development.md)
for the one-time setup (`make db-up`, `make migrate`, `make dev-key`).

**Terminal 1, the demo tools:**

```bash
go run ./examples/refund-demo/tools-server
```

**Terminal 2, Rillock:**

```bash
make run ADDR=127.0.0.1:8084
```

**Terminal 3, the CLI:**

```bash
source .rillock/env
rillock apply -f examples/refund-demo/refund.yaml

rillock run refund-demo --input-json '{"script": [
  {"toolCalls": [{"name": "orders_get", "input": {"order_id": "ord_812"}}]},
  {"toolCalls": [{"name": "payments_refund", "input": {"order_id": "ord_812", "amount": 300}}]},
  {"text": "Refunded $300 for the desk lamp."}
]}'

rillock trace <run id>
```

Terminal 1 logs each call the agent made, with its idempotency key.

## Things to try

- **A forbidden tool.** Script a call to `delete_customers`. The trace shows it
  refused (⊘), and terminal 1 shows it was never called.
- **A loop.** Add `"repeatLast": true` to the script. The run stops at
  `maxSteps` with "step limit reached".
- **A missing order.** Look up `ord_404`. The tool answers 404, the model sees
  the error (✗), and the run continues.
- **Tools down.** Stop terminal 1 and submit a run. It fails with
  "tool unreachable".
