#!/usr/bin/env python3
"""Create stable source and vendored-module archives for RPM and releases."""

import os
from pathlib import Path
import re
import shutil
import struct
import subprocess
import tarfile
import tempfile
import zlib


ROOT = Path(__file__).resolve().parents[2]
DIST = ROOT / "dist"
SPEC = ROOT / "packaging/rpm/barnacle.spec"
match = re.search(r"^Version:\s+(\S+)", SPEC.read_text(), re.MULTILINE)
if not match:
    raise SystemExit("RPM spec has no Version")
VERSION = match.group(1)
if os.environ.get("RELEASE_VERSION", VERSION) != VERSION:
    raise SystemExit(f"release tag version does not match RPM spec: {VERSION}")

try:
    timestamp = int(os.environ.get("SOURCE_DATE_EPOCH") or subprocess.check_output(
        ["git", "-c", f"safe.directory={ROOT}", "log", "-1", "--format=%ct", "HEAD"], cwd=ROOT, text=True
    ).strip())
except (OSError, subprocess.CalledProcessError, ValueError) as error:
    raise SystemExit("set SOURCE_DATE_EPOCH or build from a Git checkout") from error


def add_tree(archive, path, arcname):
    paths = [path, *sorted(path.rglob("*"))] if path.is_dir() else [path]
    for item in paths:
        if item.is_symlink():
            raise SystemExit(f"refusing symlink in release archive: {item}")
        relative = item.relative_to(path)
        name = arcname if item == path else f"{arcname}/{relative.as_posix()}"
        info = tarfile.TarInfo(name)
        info.uid = info.gid = 0
        info.uname = info.gname = ""
        info.mtime = timestamp
        if item.is_dir():
            info.type = tarfile.DIRTYPE
            info.mode = 0o755
            archive.addfile(info)
        elif item.is_file():
            info.size = item.stat().st_size
            info.mode = 0o755 if item.stat().st_mode & 0o111 else 0o644
            with item.open("rb") as contents:
                archive.addfile(info, contents)
        else:
            raise SystemExit(f"unsupported release archive entry: {item}")


def write_archive(path, tree, prefix):
    # Stored DEFLATE blocks avoid platform-specific zlib compression choices.
    with tempfile.TemporaryFile() as plain:
        with tarfile.open(fileobj=plain, mode="w", format=tarfile.PAX_FORMAT) as archive:
            add_tree(archive, tree, prefix)
        plain.seek(0)
        with path.open("wb") as output:
            output.write(bytes.fromhex("1f8b08000000000000ff"))
            checksum = size = 0
            while chunk := plain.read(65535):
                length = len(chunk)
                output.write(b"\x00" + struct.pack("<HH", length, length ^ 0xFFFF) + chunk)
                checksum = zlib.crc32(chunk, checksum)
                size = (size + length) & 0xFFFFFFFF
            output.write(b"\x01\x00\x00\xff\xff")
            output.write(struct.pack("<II", checksum, size))


with tempfile.TemporaryDirectory(prefix="barnacle-source-") as temporary:
    temp = Path(temporary)
    source = temp / f"barnacle-{VERSION}"
    source.mkdir()
    files = [
        "go.mod", "go.sum", "README.md", "LICENSE", "THIRD-PARTY-NOTICES.md",
        *[item.name for item in sorted(ROOT.glob("*.go"))],
        "packaging/rpm/barnacle.spec", "packaging/rpm/barnacle.service",
        "packaging/rpm/barnacle.sysconfig", "packaging/rpm/barnacle-key-access.conf",
    ]
    for name in files:
        target = source / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / name, target)
    shutil.copytree(ROOT / "internal", source / "internal")
    shutil.copytree(ROOT / "THIRD-PARTY-LICENSES", source / "THIRD-PARTY-LICENSES")

    vendor = temp / "vendor"
    environment = os.environ.copy()
    environment["GOTOOLCHAIN"] = "auto"
    environment["GOWORK"] = "off"
    environment["GOCACHE"] = str(temp / "go-cache")
    environment.pop("GOFLAGS", None)
    subprocess.run(["go", "mod", "vendor", "-o", str(vendor)], cwd=source, env=environment, check=True)

    DIST.mkdir(exist_ok=True)
    write_archive(DIST / f"barnacle-{VERSION}.tar.gz", source, source.name)
    write_archive(DIST / f"barnacle-{VERSION}-vendor.tar.gz", vendor, "vendor")
    print(f"Source and vendor archives written to {DIST}")
