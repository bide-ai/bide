Section: Changed

`bide approve` exits 0 with "already recorded: your approve at <time> counts; nothing to do" when the approver's verified, counted decision is the one they are making again (approve after approve, reject after reject), instead of refusing it as already decided; it signs and writes nothing, points to the existing receipt, and still refuses the opposite verdict, so a retry after a lost response is safe under every signature scheme, including the randomized ecdsa-p256.
