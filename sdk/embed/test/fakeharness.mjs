// A stand-in for `harness acp` that the subprocess tests spawn. FAKE_MODE picks
// the behavior: report (print the environment and wait for EOF), eof3 (exit 3
// at EOF), hang (ignore EOF and SIGTERM), garbage (print a line that is no
// frame), crash (exit 2 at once), noisy (write two lines to stderr).
const mode = process.env.FAKE_MODE;
const frame = (value) => process.stdout.write(JSON.stringify(value) + "\n");
process.stdin.resume();
if (mode === "report") {
  frame({ jsonrpc: "2.0", method: "report", params: { cwd: process.cwd(), arg: process.argv[2], marker: process.env.FAKE_MARKER, config: process.env.HARNESS_CONFIG, sessions: process.env.HARNESS_SESSION_DIR } });
  process.stdin.on("end", () => process.exit(0));
} else if (mode === "eof3") {
  frame({ jsonrpc: "2.0", method: "ready" });
  process.stdin.on("end", () => process.exit(3));
} else if (mode === "hang") {
  frame({ jsonrpc: "2.0", method: "ready" });
  process.on("SIGTERM", () => {});
  setInterval(() => {}, 1000);
} else if (mode === "garbage") {
  process.stdout.write("not a frame\n");
  setInterval(() => {}, 1000);
} else if (mode === "crash") {
  process.stderr.write("boom\n");
  process.exit(2);
} else if (mode === "noisy") {
  process.stderr.write("one\ntwo\n");
  frame({ jsonrpc: "2.0", method: "ready" });
  process.stdin.on("end", () => process.exit(0));
}
