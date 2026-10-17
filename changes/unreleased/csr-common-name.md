Section: Added

`bide keygen -csr` and `bide-hsm csr` take `-cn <name>`, which makes the request's subject one common name (printable ASCII text of at most 64 characters, no leading or trailing space; a bad name is a usage error before any key is used or any billed signature made). The gate reads no subject, so the name changes nothing about who may approve; it exists for tools such as `step ca sign` that refuse a request with an empty subject and take the certificate's content from the CA's template. Without `-cn` the request is unchanged: an empty subject.
