Section: Changed

The release workflow handles pre-release tags (`vX.Y.Z-rc.N`): the module set SBOM accepts a semantic version with a pre-release suffix (no leading zeros, no empty identifiers, checked the same way by `scripts/module-sbom.sh` and the release builder), the release is marked a pre-release as before, and no Homebrew formula is generated or pushed for a tag with a hyphen, so `brew` never offers a pre-release as the current version.
