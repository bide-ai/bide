Section: Fixed

v0.18.0 did not publish the module set SBOM (`bide_0.18.0_modules.sbom.json`) that its release notes described: `scripts/module-sbom.sh` wrote the file relative to the temporary module it changes into, so the release workflow's signing step did not find it. The script is fixed in #343; the v0.18.0 release notes and `docs/trust/verify-releases.md` now say that v0.18.0 has no module set SBOM, and the next release carries it.
