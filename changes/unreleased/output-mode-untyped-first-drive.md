Section: Fixed

`WithOutputMode` on an untyped run is now `ErrConfig` at the run's first drive, before anything is journaled or any model call is made, as the documentation says; only a later drive of the run refused it before.
