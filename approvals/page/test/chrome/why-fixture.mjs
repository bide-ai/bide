// why-fixture.mjs: the reasons colours.mjs has the page draw. The rig's call is a plain gate, so the endpoint's
// answer to GET /pending is given a "why" member (everything else in it is the endpoint's, byte for byte): a
// taint reason, a cap reason with figures, a conflict, and one with hostile names, so the list is drawn in every
// mode by the page's own code and measured like the rest of the page. EXPECTED is what the page writes for
// them, one item each, in order; a test in the page's script suite holds it to the page's own code
// (why-fixture.test.mjs), and the Chrome test holds the drawn list to it.

export const WHY = '[{"kind":"taint","source":"inbound-email","ref":"tool:read_email/c1","pos":4,"at":"2026-10-24T10:43:00.000Z"},' +
  '{"kind":"cap","rule":"refund_one","action":"gate","why":"over","currency":"USD","amount":600000,"limit":500000,"at":"2026-10-24T10:43:00.000Z"},' +
  '{"kind":"conflict"},' +
  '{"kind":"taint","source":"<img src=x onerror=alert(1)>\\u202eevil","ref":"input","pos":9,"at":"2026-10-24T10:43:00.000Z"}]';

// injectWhy puts the member first in the answer's object.
export const injectWhy = (body) => body.replace(/^\s*\{/, '{"why":' + WHY + ',');

export const EXPECTED = [
  'untrusted content from inbound-email (read by tool read_email, call c1, journal position 4) (as of 2026-10-24T10:43:00.000Z)',
  'cap refund_one: this call 6000.00 USD, limit 5000.00 USD (over, gate) (as of 2026-10-24T10:43:00.000Z)',
  'conflicting reason records for this call: none is shown, and the journal should be looked at',
  "untrusted content from <img src=x onerror=alert(1)>\\u202eevil (the run's input, journal position 9) (as of 2026-10-24T10:43:00.000Z)",
];
