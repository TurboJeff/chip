Builds for macOS (Apple Silicon and Intel) and Linux (x86-64 and ARM64). Each archive contains the `chip` binary and the license files. To build for another platform, see Building in the README.

## Install

Download the archive for your machine, check it against `SHA256SUMS`, and unpack it:

```sh
# macOS: shasum -a 256 -c SHA256SUMS --ignore-missing
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf chip_*.tar.gz
sudo install chip_*/chip /usr/local/bin/chip
chip -version
```

The macOS binaries aren't signed by Apple. If you downloaded the archive with a browser, macOS blocks the first run. Remove the quarantine flag with `xattr -d com.apple.quarantine /usr/local/bin/chip`. Archives downloaded with `curl` aren't affected.

Then register a chip at https://hub.chipin.dev and follow the Quick start in the README.
