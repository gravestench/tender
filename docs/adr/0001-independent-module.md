# 0001. Independent module and permissive license

**Status** Accepted.

Tender's module path is `github.com/gravestench/tender`, matching its GitHub origin. The root module contains reusable Go APIs, uses Go 1.26 as its minimum version, and favors the standard library. Use the latest patched toolchain for development and CI. Hosts supply runtime configuration and, for PostgreSQL, an injected `database/sql` driver. Record other dependencies with a rationale and license review before adoption.

Use 0BSD because it permits commercial and private reuse, modification, and redistribution without a notice-preservation condition. [SPDX 0BSD](https://spdx.org/licenses/0BSD.html)

This boundary allows Blacktop and other consumers to control their own deployment and use the same public package. A hosted middleware service would impose a deployment dependency. Embedding Tender in Blacktop would make independent import, testing, and versioning harder.
