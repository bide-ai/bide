Section: Changed

The Go tests that drive a page in Chrome through a Node script (the editor, the run viewer and the approve page) now run the script under a five-minute deadline and, when it is not done by then, kill it together with the Chrome it started and fail with what the script had printed, instead of waiting for the test binary's own timeout (one such run hung for 564 seconds with nothing said). This is test infrastructure only; it changes no behaviour of the product.
