// stall.mjs: prints a line and waits for ever. TestScriptsHaveADeadlineInChrome runs it to check that a
// script that stalls is killed at the deadline instead of holding the test run.
console.error('stalled on purpose');
setInterval(() => {}, 1 << 30);
