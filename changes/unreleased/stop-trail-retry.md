Section: Fixed

The stop trail's append now retries after losing a race for its position until its caller's deadline (30 seconds when the caller has none), waiting a jittered, growing time between tries, instead of giving up after eight tries. With ten operators writing at once the eight tries ran out (under the race detector, in a test), and a trail write that gives up turns a stop that is in force into exit 5 of `bide stop`; thirty-two concurrent writers now all land.
