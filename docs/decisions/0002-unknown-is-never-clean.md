# 0002: Unknown is never clean

**Decision:** input that can't be trusted exits 3 with no result. A pool size, pod count or limit that can't be read, or is assumed, is a warning (TC9xx) and the result line says it isn't clean.

**Why:** the tool's value is the claim that the pools fit. A wrong "fits" is worse than no answer, and earlier tools from this account reported failed builds as all clear. Empty, binary, cut-off and failed-command files are refused, and folder walks refuse broken YAML too, because `helm template > rendered.yaml` that fails leaves an empty or error file behind.
