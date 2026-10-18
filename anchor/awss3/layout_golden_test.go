package awss3_test

import "testing"

// The object layout is a wire format: a head anchored today must be found and verified by a reader of
// any later version, so a name or a body that changes by one byte orphans every head already
// anchored. These values were produced before the layout code moved to audit/anchorobj, and the move
// must not change them.
func TestObjectLayoutIsByteIdentical(t *testing.T) {
	k := newKeys(t, 1)
	for _, c := range []struct {
		run        string
		size       int
		ts         int64
		key, body string
	}{
		{"run-1", 3, 100,
			"bide-anchor/08b41df3b7d966b1d62a971e7b5c3c935bdda6fc5588147e435ff58c8ae7878f/9223372036854775804-9adb9139463679b754868f72a2fc69d2.json",
			`{"format":"bide.audit.s3-anchor.v1","sth":{"format":"bide.audit.sth.v5","kind":"journal","run_id":"run-1","size":3,"root":"JnMrIfIX6UBvWniuR9Y4Mwt41dePMTKMAxK9Usa+sFE=","timestamp_nanos":100,"alg":"ed25519","signature":"iUR5o5YlQSwSZR1tLZ4LmWhXs8+Ad5x2UlGycA9H7rIq6wXrqpKmtx6fFNeQfXX+CI6dyJ5aR/Vs3AZ+K6KpDQ=="}}`},
		{"run-1", 0, 1,
			"bide-anchor/08b41df3b7d966b1d62a971e7b5c3c935bdda6fc5588147e435ff58c8ae7878f/9223372036854775807-34060f52b2c1984d1e543de0dcda98f8.json",
			`{"format":"bide.audit.s3-anchor.v1","sth":{"format":"bide.audit.sth.v5","kind":"journal","run_id":"run-1","size":0,"root":"gUJnVVxbFgaWSKOfXCS0yRI9OPrJWtl60L6/UCYTrrg=","timestamp_nanos":1,"alg":"ed25519","signature":"IjP8QNtI58FKBvYOenV/MNO9d/siulvZrPSGEwujefLBkWyR7ADp7U34B4SaCNqYcpVZXwnU7E4UA5QfocQjCQ=="}}`},
		{"notify:@open:2026-10-17", 123456, 99,
			"bide-anchor/301152be4769aca60b8e0351b66c8fdc70847918e70a9a0587a210bd7425ebd6/9223372036854652351-5812d23c0beaa41a1da8ecf211a9e44a.json",
			`{"format":"bide.audit.s3-anchor.v1","sth":{"format":"bide.audit.sth.v5","kind":"journal","run_id":"notify:@open:2026-10-17","size":123456,"root":"iXJwO2eFmAh3Z/KrCm1o0DpNY4GGPHc+kdCfZIdeR3g=","timestamp_nanos":99,"alg":"ed25519","signature":"Tv/6yZhxrC7+k3hBEB+dvFQfg79sS139cZsjurSy5KoR3G23tzLJl2pe0wxWosinCQwAkdRDlhgigEioZ+cqCg=="}}`},
	} {
		key, body := keyAndBody(t, k, k.head(t, c.run, c.size, c.ts))
		if key != c.key {
			t.Errorf("%q size %d: key %s\n want %s", c.run, c.size, key, c.key)
		}
		if string(body) != c.body {
			t.Errorf("%q size %d: body\n %s\n want\n %s", c.run, c.size, body, c.body)
		}
	}
}
