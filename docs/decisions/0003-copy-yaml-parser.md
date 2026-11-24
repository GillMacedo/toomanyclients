# 0003: Copy the YAML parser into the package

**Decision:** `plugin/scripts/yaml_*.go` is go.yaml.in/yaml/v3 v3.0.4, renamed to `package main` and gofmt'd, with its Apache 2.0 and MIT license files next to it (`YAML_LICENSE`, `YAML_NOTICE`). The copy comes from this account's `overselect` repository, which made the same choice.

**Why:** the plugin folder is installed on its own, without `go.mod`, so `run.sh` builds the files by name and can't download modules. A hand-written YAML reader would get multi-document files, anchors, flow mappings and block scalars wrong.

**Rule:** don't edit these files. To update, copy a new release and rename the package again.
