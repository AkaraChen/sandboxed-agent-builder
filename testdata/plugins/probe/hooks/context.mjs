import { readFileSync, writeFileSync } from "node:fs";
const event = JSON.parse(readFileSync(0, "utf8"));
writeFileSync("/tmp/probe-prompt.json", JSON.stringify(event));
process.stdout.write(JSON.stringify({ context: "The hook codeword is ZEBRA-42." }));
