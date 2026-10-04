#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
for arch in amd64 arm64; do
    test -f "$repo_dir/dist/hermit-linux-$arch" || { echo "missing linux/$arch binary; run packaging/release/build.sh first" >&2; exit 1; }
done
builder="hermit-release-$$"
docker buildx create --name "$builder" --driver docker-container >/dev/null
trap 'docker buildx rm "$builder" >/dev/null 2>&1 || true' EXIT HUP INT TERM
docker buildx build --builder "$builder" --platform linux/amd64,linux/arm64 \
    --file "$repo_dir/Dockerfile.release" \
    --sbom=true --provenance=mode=max \
    --output "type=oci,dest=$repo_dir/dist/hermit-image.oci.tar" \
    "$repo_dir"
python3 - "$repo_dir/dist/hermit-image.oci.tar" <<'PY'
import json
import sys
import tarfile

with tarfile.open(sys.argv[1]) as archive:
    def read_blob(digest):
        return json.load(archive.extractfile("blobs/sha256/" + digest.split(":", 1)[1]))

    root = json.load(archive.extractfile("index.json"))
    index = read_blob(root["manifests"][0]["digest"])
    platforms = {item.get("platform", {}).get("architecture") for item in index["manifests"]}
    attestations = [item for item in index["manifests"] if item.get("annotations", {}).get("vnd.docker.reference.type") == "attestation-manifest"]
    predicates = set()
    for item in attestations:
        manifest = read_blob(item["digest"])
        predicates.update(layer.get("annotations", {}).get("in-toto.io/predicate-type") for layer in manifest["layers"])
    if not {"amd64", "arm64"}.issubset(platforms) or not {"https://spdx.dev/Document", "https://slsa.dev/provenance/v1"}.issubset(predicates):
        raise SystemExit("OCI archive is missing a target platform, SBOM, or provenance")
PY
echo "Attested multi-platform OCI image written to $repo_dir/dist/hermit-image.oci.tar"
