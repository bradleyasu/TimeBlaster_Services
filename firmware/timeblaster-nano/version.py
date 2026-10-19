# PlatformIO pre-build script: stamp the firmware with its version.
#
# The version is the last commit that touched the firmware's own source -- not
# the repository's HEAD. A change to the Pi's Go code must not make the Nano's
# firmware look out of date, or the app would offer an "update" after every
# deploy that leaves the firmware byte-for-byte the same.
#
# Uncommitted changes to those paths add "-dirty", so an image built from a
# working tree can never be mistaken for the committed release.
#
# The same version is written beside the image, so the release step copies a
# version that is guaranteed to match what was compiled in.
Import("env")  # noqa: F821  (provided by PlatformIO)

import os
import subprocess

PROJECT = env.subst("$PROJECT_DIR")  # noqa: F821
SOURCES = ["src", "platformio.ini", "version.py"]


def git(*args):
    try:
        out = subprocess.run(
            ["git", "-C", PROJECT, *args], capture_output=True, text=True, check=True
        )
        return out.stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        return ""


version = git("log", "-1", "--format=%h", "--", *SOURCES) or "unknown"
if git("status", "--porcelain", "--", *SOURCES):
    version += "-dirty"

env.Append(CPPDEFINES=[("FIRMWARE_VERSION", env.StringifyMacro(version))])  # noqa: F821

build_dir = env.subst("$BUILD_DIR")  # noqa: F821
os.makedirs(build_dir, exist_ok=True)
with open(os.path.join(build_dir, "firmware.version"), "w") as fh:
    fh.write(version + "\n")

print("Timeblaster firmware version: " + version)
