#!/usr/bin/env python3
"""
Compatibility shim. Do not add logic here.

App builds from before the Go rewrite start the supervisor with
`python /opt/cp/supervisor.py` (see provision.ts in the app). The supervisor is
now the `cpd` binary; this hands over to it, keeping the environment and
replacing the process so signals and the exit status behave as before.

Delete once no released app build launches this path.
"""
import os

os.execv("/usr/local/bin/cpd", ["cpd", "serve"])
