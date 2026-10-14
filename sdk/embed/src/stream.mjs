// The ACP stream of a Harness, shared by every transport.

/**
 * acpStream returns {readable, writable, emit, end}. A frame that the page
 * writes goes to send, which throws when the connection is closed. close ends
 * the connection: it runs when the page closes the writable or cancels the
 * readable. emit queues a frame for the page; end closes the readable, or
 * errors it when it gets an error.
 */
export function acpStream({ send, close }) {
  let controller;
  let open = true;
  const readable = new ReadableStream({
    start: (c) => (controller = c),
    cancel() {
      open = false;
      return close();
    },
  });
  const writable = new WritableStream({ write: send, close, abort: close });
  return {
    readable,
    writable,
    emit(frame) {
      if (!open) return;
      try {
        controller.enqueue(frame);
      } catch {
        open = false;
      }
    },
    end(err) {
      if (!open) return;
      open = false;
      try {
        if (err) controller.error(err);
        else controller.close();
      } catch {
        // The reader canceled the stream.
      }
    },
  };
}
