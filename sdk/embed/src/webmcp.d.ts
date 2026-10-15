import type { PageTool } from "./index.js";

export interface WebmcpToolsOptions {
  /** Starts each tool name. Default "webmcp__". */
  prefix?: string;
  /** Adds the tools that frames of these origins expose to the page. Default none. */
  fromOrigins?: string[];
  /** Receives a JSON log line for each tool that is left out and for a getTools failure. Default: no logging. */
  log?: (line: string) => void;
}

/**
 * Returns the WebMCP tools of the page (document.modelContext.getTools) as page tools, named prefix plus the WebMCP name.
 * It returns an empty list where the page has no WebMCP or getTools rejects, and it never rejects.
 * The list is a snapshot; a tool with an invalid name or schema is left out.
 */
export function webmcpTools(options?: WebmcpToolsOptions): Promise<PageTool[]>;
