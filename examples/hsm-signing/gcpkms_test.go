package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	emulator "github.com/blackwell-systems/gcp-kms-emulator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/bide-ai/bide/signer/gcpkms"
)

// emulatedKMS starts the Cloud KMS emulator in the test process, points newKMSClient at it, and
// returns the name of a key version with the algorithm.
func emulatedKMS(t *testing.T, alg kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) string {
	t.Helper()
	t.Setenv("IAM_MODE", "off")
	srv := grpc.NewServer()
	if err := emulator.Register(srv); err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	client, err := kms.NewKeyManagementClient(context.Background(), option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	old := newKMSClient
	newKMSClient = func(context.Context) (gcpkms.API, func(), error) { return client, func() {}, nil }
	t.Cleanup(func() { newKMSClient = old })

	ctx := context.Background()
	ring, err := client.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/bank-prod/locations/europe-west2", KeyRingId: "bide"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := client.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.Name, CryptoKeyId: "audit",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg}}})
	if err != nil {
		t.Fatal(err)
	}
	return key.Primary.Name
}

func TestRunWithCloudKMSP256(t *testing.T) {
	version := emulatedKMS(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	env := map[string]string{"BIDE_GCPKMS_KEY_VERSION": version, "BIDE_GCPKMS_ALLOW_SOFTWARE_KEY": "1"} // P-256 is the default scheme
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"log public key: ecdsa-p256:", "signature valid, root is the journal's root", "verified with the public key alone"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRunWithCloudKMSEd25519(t *testing.T) {
	version := emulatedKMS(t, kmspb.CryptoKeyVersion_EC_SIGN_ED25519)
	env := map[string]string{"BIDE_GCPKMS_KEY_VERSION": version, "BIDE_GCPKMS_SCHEME": "ed25519", "BIDE_GCPKMS_ALLOW_SOFTWARE_KEY": "1"}
	var out bytes.Buffer
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "log public key: ed25519:") {
		t.Errorf("output:\n%s", out.String())
	}
}

// The emulator's keys are software keys, so without BIDE_GCPKMS_ALLOW_SOFTWARE_KEY they are refused, and a
// scheme the key version is not is refused too.
func TestCloudKMSRefusals(t *testing.T) {
	version := emulatedKMS(t, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
	var out bytes.Buffer
	env := map[string]string{"BIDE_GCPKMS_KEY_VERSION": version}
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); !errors.Is(err, gcpkms.ErrProtectionLevel) {
		t.Fatalf("a software key by default: %v", err)
	}
	env["BIDE_GCPKMS_ALLOW_EXTERNAL_KEY"] = "1" // external is not software
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); !errors.Is(err, gcpkms.ErrProtectionLevel) {
		t.Fatalf("a software key with only external allowed: %v", err)
	}
	env = map[string]string{"BIDE_GCPKMS_KEY_VERSION": version, "BIDE_GCPKMS_SCHEME": "ed25519", "BIDE_GCPKMS_ALLOW_SOFTWARE_KEY": "1"}
	if err := run(context.Background(), func(k string) string { return env[k] }, &out); !errors.Is(err, gcpkms.ErrNoEd25519) {
		t.Fatalf("a P-256 key under the ed25519 scheme: %v", err)
	}
}

func TestCloudKMSClientError(t *testing.T) {
	old := newKMSClient
	newKMSClient = func(context.Context) (gcpkms.API, func(), error) { return nil, nil, errors.New("no credentials") }
	t.Cleanup(func() { newKMSClient = old })
	env := map[string]string{"BIDE_GCPKMS_KEY_VERSION": "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"}
	if err := run(context.Background(), func(k string) string { return env[k] }, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "cloud kms client") {
		t.Fatalf("err = %v", err)
	}
}
