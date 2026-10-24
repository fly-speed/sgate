#!/usr/bin/env python3
"""Generate standard Go vendor packages without deleting local reference repos."""
import os
import pathlib
import shutil
import subprocess
import tempfile

root = pathlib.Path(__file__).resolve().parents[1]
env = os.environ.copy()
env.update(GOFLAGS="-mod=mod")
env.setdefault("GOPROXY", "off")
with tempfile.TemporaryDirectory(prefix="sgate-vendor-") as temp:
    stage = pathlib.Path(temp) / "vendor"
    subprocess.run(["go", "mod", "vendor", "-o", str(stage)], cwd=root, env=env, check=True)
    destination = root / "vendor"
    destination.mkdir(exist_ok=True)
    for source in stage.iterdir():
        target = destination / source.name
        if source.is_dir():
            shutil.copytree(source, target, dirs_exist_ok=True)
        else:
            shutil.copyfile(source, target)
print(
    "standard vendor synchronized; vendor/gin-service and vendor/go-service "
    "reference repositories preserved"
)
