"""Select working-tree Go tests for the optional workstation gate."""

import json
import os
from pathlib import Path
import subprocess
import sys


def changed_directories():
    subprocess.check_output(["git", "rev-parse", "--verify", "HEAD"])
    status = subprocess.check_output([
        "git", "status", "--porcelain=v1", "-z", "--untracked-files=all",
        "--no-renames", "--ignore-submodules=none",
    ])
    records = status.split(b"\0")
    if records.pop():
        return None
    directories = set()
    for record in records:
        flags = record[:2]
        # With rename detection disabled, a move includes a deleted path.
        if record[2:3] != b" " or (flags != b"??" and any(flag not in b" MA" for flag in flags)):
            return None
        path = Path(os.fsdecode(record[3:]))
        if path.suffix != ".go" or path.is_symlink() or not path.is_file():
            return None
        directories.add(path.parent.resolve())
    return directories


def affected_packages(directories):
    workspace = subprocess.check_output(["go", "env", "GOWORK"]).strip()
    if workspace not in (b"", b"off"):
        return None

    listing = subprocess.check_output([
        "go", "list", "-json=Dir,ImportPath,Imports,TestImports,XTestImports", "./...",
    ], text=True)
    package_by_directory = {}
    imports_by_package = {}
    decoder = json.JSONDecoder()
    listing = listing.lstrip()
    while listing:
        package, end = decoder.raw_decode(listing)
        name = package["ImportPath"]
        package_by_directory[Path(package["Dir"]).resolve()] = name
        imports_by_package[name] = set(package.get("Imports", []))
        imports_by_package[name].update(package.get("TestImports", []), package.get("XTestImports", []))
        listing = listing[end:].lstrip()

    if not directories <= package_by_directory.keys():
        return None
    affected = {package_by_directory[directory] for directory in directories}
    while True:
        dependents = {name for name, imports in imports_by_package.items() if imports & affected}
        if dependents <= affected:
            return sorted(affected)
        affected.update(dependents)


def main():
    try:
        directories = changed_directories()
        packages = affected_packages(directories) if directories else None
    except (OSError, ValueError, TypeError, KeyError, subprocess.CalledProcessError):
        packages = None

    if not packages:
        print("check-fast: running the full gate for this change", flush=True)
        return subprocess.call(["just", "check"], stdin=subprocess.DEVNULL)

    print("check-fast: testing " + ", ".join(packages), flush=True)
    status = subprocess.call(["just", "fmt-check", "lint"], stdin=subprocess.DEVNULL)
    if status != 0:
        return status
    return subprocess.call(["go", "test", "-race", *packages], stdin=subprocess.DEVNULL)


if __name__ == "__main__":
    sys.exit(main())
