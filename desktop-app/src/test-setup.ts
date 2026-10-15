// Runs in each test worker before its test file. A worker's first
// number formatting sets up ICU, which takes up to five seconds on the
// Windows CI runners; done here, that cost lands outside every test's
// timeout instead of in whichever test formats a number first.
(0).toLocaleString("en-US");
