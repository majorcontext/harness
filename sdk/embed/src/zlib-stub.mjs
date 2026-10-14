// A stand-in for node:zlib in a browser bundle of just-bash. just-bash/browser
// imports these names for gzip, gunzip, and zcat, which cannot run in a page.
// Alias node:zlib to this file in the bundler; no other command uses it.
const unavailable = () => {
  throw new Error("gzip is not available in the browser");
};

export const gzipSync = unavailable;
export const gunzipSync = unavailable;
export const constants = {};
export default { gzipSync, gunzipSync, constants };
