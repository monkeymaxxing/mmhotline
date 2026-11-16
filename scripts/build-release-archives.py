#!/usr/bin/env python3
"""Build MMHotline's release archives and their SHA256SUMS from one commit.

The development design, docs/changing-mmhotline/design/development.md, section "Release
archives", describes the archives and the reasons for their layout.
"""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

REPOSITORY = Path(__file__).resolve().parents[1]
# Each platform as the archive names it, and the GOOS and GOARCH of its
# program: Linux for the server, and macOS for a responder's laptop, which
# posts to the status page while the server is down.
PLATFORMS = {
    "linux-amd64": ("linux", "amd64"),
    "linux-arm64": ("linux", "arm64"),
    "macos-amd64": ("darwin", "amd64"),
    "macos-arm64": ("darwin", "arm64"),
}
LICENSE_DIR = "dependency_license_notices"
# The files at a module's root that its license asks to pass on with the
# program, such as LICENSE.txt, LICENSE-SQLITE, and NOTICE.
LICENSE_FILE_PREFIXES = ("LICENSE", "LICENCE", "COPYING", "NOTICE")


def validate_version(version):
    """A semantic version without a v prefix, such as 0.1.0 or 0.1.0-rc.1."""
    number = r"(?:0|[1-9][0-9]*)"
    identifier = r"(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
    if not re.fullmatch(rf"{number}\.{number}\.{number}(?:-{identifier}(?:\.{identifier})*)?", version):
        raise ValueError(f"Version {version!r} must be a semantic version without a v prefix, such as 0.1.0 or 0.1.0-rc.1.")


def git(*arguments, directory=REPOSITORY):
    return subprocess.check_output(["git", "-C", str(directory), *arguments], text=True).strip()


def resolve_commit(commit):
    result = subprocess.run(["git", "-C", str(REPOSITORY), "rev-parse", "--verify", "--quiet", commit + "^{commit}"],
                            capture_output=True, text=True)
    if result.returncode:
        raise ValueError(f"{commit!r} names no commit in {REPOSITORY}.")
    return result.stdout.strip()


def check_out(commit, source):
    """Clone the repository and check out the commit. Uncommitted changes and
    untracked files never reach a release, and Go records the commit and an
    unmodified checkout in the program, which `mmhotline version` prints."""
    git_dir = REPOSITORY / git("rev-parse", "--git-common-dir")
    subprocess.run(["git", "clone", "--quiet", "--no-checkout", str(git_dir), str(source)], check=True)
    subprocess.run(["git", "-C", str(source), "checkout", "--quiet", "--detach", commit], check=True)


def go_environment(source):
    """Build with the toolchain go.mod names, never a newer Go on this machine,
    with cgo off as the development design requires, and without this
    machine's GOFLAGS or a go.work file, which could change the build."""
    toolchain = re.search(r"^toolchain (\S+)$", (source / "go.mod").read_text(), re.MULTILINE)
    if not toolchain:
        raise ValueError("go.mod names no toolchain; a release is built with the toolchain go.mod names.")
    return dict(os.environ, GOTOOLCHAIN=toolchain.group(1), CGO_ENABLED="0", GOFLAGS="", GOWORK="off")


def build_program(source, environment, version, commit, platform, destination):
    """Build the platform's program and return its build information, after
    checking that the build is the one the release names."""
    system, architecture = PLATFORMS[platform]
    subprocess.run(["go", "build", "-trimpath", "-ldflags", f"-X main.version={version}",
                    "-o", str(destination), "./cmd/mmhotline"],
                   cwd=source, env=dict(environment, GOOS=system, GOARCH=architecture), check=True)
    build_information = json.loads(subprocess.check_output(
        ["go", "version", "-m", "-json", str(destination)], cwd=source, env=environment, text=True))
    settings = {setting["Key"]: setting["Value"] for setting in build_information["Settings"]}
    expected = {"CGO_ENABLED": "0", "GOOS": system, "GOARCH": architecture,
                "vcs.revision": commit, "vcs.modified": "false"}
    different = {key: settings.get(key) for key, value in expected.items() if settings.get(key) != value}
    if different:
        raise ValueError(f"The {platform} program's build information differs from the release's {expected}: {different}")
    return build_information


def license_files(source, environment, build_information):
    """Map each archive path under dependency_license_notices/ to its file:
    Go's license, since the standard library is compiled into the program,
    and the license files of each module that the program's own build
    information lists, which are exactly the modules compiled into it."""
    goroot = Path(subprocess.check_output(["go", "env", "GOROOT"], cwd=source, env=environment, text=True).strip())
    files = {f"{LICENSE_DIR}/go/LICENSE": goroot / "LICENSE"}
    module_paths = [module["Path"] for module in build_information["Deps"]]
    listing = subprocess.check_output(["go", "list", "-m", "-f", "{{.Path}} {{.Dir}}", *module_paths],
                                      cwd=source, env=environment, text=True)
    for line in listing.splitlines():
        module_path, _, module_directory = line.partition(" ")
        if not module_directory:
            raise ValueError(f"Go has no downloaded copy of module {module_path}.")
        found = sorted(path for path in Path(module_directory).iterdir()
                       if path.is_file() and path.name.upper().startswith(LICENSE_FILE_PREFIXES))
        if not found:
            raise ValueError(f"Module {module_path} has no license file at its root; find its license before releasing.")
        for path in found:
            files[f"{LICENSE_DIR}/{module_path}/{path.name}"] = path
    return files


def write_archive(destination, folder, files, timestamp):
    """Write a gzip-compressed tar of one folder from a map of path inside the
    folder to contents. Owners, modes, times, and order are fixed, so the
    same commit and version always give the same bytes."""
    entries = {folder: None}
    for name in files:
        for parent in Path(name).parents:
            if parent != Path("."):
                entries[f"{folder}/{parent.as_posix()}"] = None
    entries.update({f"{folder}/{name}": contents for name, contents in files.items()})
    with destination.open("wb") as output, \
            gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=timestamp) as compressed, \
            tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
        for name, contents in sorted(entries.items()):
            entry = tarfile.TarInfo(name)
            entry.mtime = timestamp
            entry.uname = entry.gname = "root"
            if contents is None:
                entry.type = tarfile.DIRTYPE
                entry.mode = 0o755
            else:
                entry.size = len(contents)
                entry.mode = 0o755 if name == f"{folder}/mmhotline" else 0o644
            archive.addfile(entry, None if contents is None else io.BytesIO(contents))


def build_release(version, commit, output_dir):
    validate_version(version)
    commit = resolve_commit(commit)
    output_dir = output_dir.resolve()
    output_names = [f"mmhotline-{version}-{platform}.tar.gz" for platform in PLATFORMS] + ["SHA256SUMS"]
    existing = [name for name in output_names if (output_dir / name).exists()]
    if existing:
        raise ValueError(f"{output_dir} already holds {', '.join(existing)}; a release is never overwritten.")
    with tempfile.TemporaryDirectory(prefix="mmhotline-release-") as temporary:
        build_dir = Path(temporary)
        source = build_dir / "source"
        check_out(commit, source)
        environment = go_environment(source)
        timestamp = int(git("show", "-s", "--format=%ct", commit))
        checksums = {}
        for platform in PLATFORMS:
            program = build_dir / f"mmhotline-{platform}"
            build_information = build_program(source, environment, version, commit, platform, program)
            files = {"mmhotline": program.read_bytes(), "LICENSE": (source / "LICENSE").read_bytes()}
            files.update({name: path.read_bytes()
                          for name, path in license_files(source, environment, build_information).items()})
            folder = f"mmhotline-{version}-{platform}"
            archive = build_dir / f"{folder}.tar.gz"
            write_archive(archive, folder, files, timestamp)
            checksums[archive.name] = hashlib.sha256(archive.read_bytes()).hexdigest()
        (build_dir / "SHA256SUMS").write_text("".join(f"{digest}  {name}\n" for name, digest in sorted(checksums.items())))
        # Nothing reaches the output directory until every file is built.
        output_dir.mkdir(parents=True, exist_ok=True)
        for name in output_names:
            shutil.move(build_dir / name, output_dir / name)
    print(f"Built MMHotline {version} from commit {commit} with {build_information['GoVersion']}:")
    print((output_dir / "SHA256SUMS").read_text(), end="")
    print(f"The archives and SHA256SUMS are in {output_dir}.")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--version", required=True,
                        help="the release's semantic version without a v prefix, such as 0.1.0")
    parser.add_argument("--commit", required=True,
                        help="the commit to build, such as v0.1.0 or a commit hash; the build uses a clean checkout of it")
    parser.add_argument("--output-dir", required=True, type=Path,
                        help="where the archives and SHA256SUMS go; created when missing, and never overwritten")
    arguments = parser.parse_args()
    try:
        build_release(arguments.version, arguments.commit, arguments.output_dir)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Release build failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
