Section: Fixed

The notifier's holder no longer sends the overdue notice from its own round while a peer's claim on the notice is live, so a slow holder and a peer no longer both send it; the holder sends it in a later round if the peer's send never lands.
