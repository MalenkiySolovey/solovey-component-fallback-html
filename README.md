# Solovey Fallback Sites

Fallback Sites provides passive public websites, templates, draft editing, validated assets, immutable publication archives, rollback, redirects and runtime health for Solovey UI. It is an in-process component with ID `fallback-html`, disabled by default.

The Go module owns the feature models and algorithms, publication rendering and integrity checks, request handling, bounded drain, generation fencing and durable lifecycle sequencing. Integration supplies typed `service.Store`, `service.Content`, `service.HostFacts`, `service.HTTPServing` and public-surface adapters. Solovey Backend retains authentication, database transactions, resource reservations, certificates, sockets, trusted client addresses, request limits and backup/restore authority. Construct the feature with `service.New` and compose one `service.Runtime` through `service.NewRuntime`.

The frontend contributes the existing `/fallback-html` route, Classic and Nexus navigation, editor and English/Russian locales through `@solovey/component-ui` and its `/api` facade. It uses the authenticated client supplied by Solovey UI. `HOST_UI_PIN.json` identifies the exact public SDK source and locked frontend dependencies used for independent qualification.

Use Go 1.26.11 or newer. Qualification runs with `GOWORK=off`:

```sh
go mod download
go test -race -count=1 ./...
go vet ./...
go mod tidy -diff
go test -tags minimal ./...
node scripts/check-source.mjs
```

To qualify the frontend, check out the exact Solovey UI commit in `HOST_UI_PIN.json`, install its locked frontend dependencies with `npm ci`, then run `node scripts/check-frontend.mjs /path/to/solovey-ui/frontend`. The script performs independent type checks, contribution/logic/locale tests and a Vite library build including dynamic view chunks and CSS. Generated bundles are disposable build outputs.

`component.json` preserves the ComponentHost descriptor, fifteen durable table identities and existing schema/migration versions. The application embeds the descriptor and pins immutable Go and frontend source inputs through its component source lock. This repository does not perform component installation or product updates.

Derived source and original authors are attributed in `NOTICE` and `SOURCE_PROVENANCE.json`; the filtered component Git ancestry is retained. Distributed under the original GNU GPL version 3 license in `LICENSE`.
