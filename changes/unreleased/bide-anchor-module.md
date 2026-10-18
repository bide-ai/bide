Section: Changed

The bide-anchor command moves out of the anchor/awss3 module into its own module, github.com/bide-ai/bide/cmd/bide-anchor (install it with go install github.com/bide-ai/bide/cmd/bide-anchor@latest), and gains -backend, which names the object store the anchor wrote to; s3, the default, is the only backend built in, and others are added with their packages. It has not been in a release, so no published install path changes; the module is tagged with the others from the next release.
