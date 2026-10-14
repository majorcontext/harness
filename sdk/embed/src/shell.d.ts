import type { PageTool } from "./index.js";

/** The part of the just-bash module that the shell tool uses. */
export interface JustBashModule {
  Bash: new (options: object) => { exec(command: string, options?: { signal?: AbortSignal }): Promise<{ stdout: string; stderr: string; exitCode: number }> };
  InMemoryFs: new () => object;
  MountableFs: new (options: object) => object;
}

export interface ShellToolOptions {
  /** The tool name. Default bash. */
  name?: string;
  /** Returns the just-bash module. Default: import("just-bash/browser"), which a bundler or an import map resolves. */
  load?: () => Promise<JustBashModule>;
  /** The wall time limit of one call in ms. Default 10000. */
  timeoutMs?: number;
  /** The limit of commands and of loop iterations of one call. Default 10000. */
  maxCommands?: number;
  /** The limit of characters of stdout and of stderr in a result; the head and the tail stay. Default 20000. */
  maxOutput?: number;
}

/**
 * A page tool with the input {command} that runs a just-bash shell over h.fs, mounted at /work (the working directory) and at /tmp when the module has it.
 * The result is stdout, stderr, and the exit code. The shell has no network. Each call starts a new shell. just-bash is an optional peer dependency.
 * The call rejects when the harness has no h.fs.
 */
export function shellTool(options?: ShellToolOptions): PageTool;
