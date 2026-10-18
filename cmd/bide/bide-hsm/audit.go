package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/auditstores"
	"github.com/bide-ai/bide/internal/auditcli"
)

// logKeyRefHelp is the -log-key help of bide-hsm audit stops.
const logKeyRefHelp = "reference to the log key in an HSM or KMS (a pkcs11: URI, awskms:<key ARN>, gcpkms:<key version> or azurekv:<key identifier>; quote it, and give a PKCS#11 PIN by pin-source, never in the reference); -pubkey names its public half, and the head is signed only if the key is that key"

// auditCmd is bide-hsm audit: bide audit stops, whose -sign-head signs with a key held in an HSM or a KMS.
// It is the same verb as bide audit stops (the verification, the exit table, the head file, the refusals), run
// by internal/auditcli, with -log-key naming the key by reference and the key flags of bide-hsm approve added.
// No other bide audit verb signs anything, so no other verb is offered here.
func auditCmd(args []string, stdout, stderr io.Writer, d deps) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprint(stdout, auditUsage)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, auditUsage)
		return 2
	}
	var kf keyFlags
	openers := d.auditStores
	if openers == nil {
		openers = auditstores.Openers()
	}
	return auditcli.MainWith(version, args, stdout, stderr, auditcli.Options{
		Program:       "bide-hsm audit",
		OnlyVerb:      "stops",
		UsageText:     auditUsage,
		LogKeyHelp:    logKeyRefHelp,
		LogKeyFlags:   func(fs *flag.FlagSet) { kf = addKeyFlags(fs) },
		LoadLogSigner: func(ref string) (audit.Signer, error) { return kf.open(ref, d) },
		Stores:        openers,
	})
}

const auditUsage = `bide-hsm audit: bide audit stops, with the log key in an HSM or a cloud KMS

  bide-hsm audit stops (-db <file> | -postgres-env <var>) -stop-policy <file> -pubkey <hex|file>
                       -sign-head -log-key <key ref> -head-out <file> [flags]

This is bide audit stops (see bide audit -h and the bide command reference): it verifies the kill switch's
stop trail against the pinned policy and the control run, and with -sign-head signs a head over exactly the
records it verified and appends it to -head-out. The differences are in the key: -log-key is a reference to a
key that stays in an HSM or a KMS, not a key file, and the flags of bide-hsm approve that say how a key is
opened (-alg, -allow-extractable, -allow-software-key, -verify-attestation) apply. The key is opened only
when there is something to sign: an audit that is not clean signs nothing and opens no key, and a run with
nothing new (the newest head already covers every record) opens none either. The exit status is that of bide
audit stops (0 verified, 1 not verified, 2 usage, 3 no verdict, 4 unusable input).

` + keyRefHelp
