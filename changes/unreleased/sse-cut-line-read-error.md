Section: Fixed

A model stream that is cut partway through an event (a dropped connection) now ends with the read error, retried as before, in every adapter that reads SSE (Anthropic, OpenAI, Gemini). Before, the half event the cut left was decoded first, and the turn failed with a misleading "unexpected end of JSON input" decode error in place of the failed read. A reply over the response cap was already handled this way.
