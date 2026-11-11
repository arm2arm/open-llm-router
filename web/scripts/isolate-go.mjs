// Some npm tooling ships Go examples. Keep go test/vet ./... scoped to this
// repository by making node_modules a separate Go module boundary.
import { writeFileSync } from 'node:fs'
writeFileSync(
  'node_modules/go.mod',
  'module olr.invalid/frontend-dependencies\n',
)
