const createForm = document.querySelector("#create-link");
const tokens = document.querySelector("#link-tokens");
const links = document.querySelector("#generated-links");
const status = document.querySelector("#link-status");
const toast = document.querySelector("#copy-toast");
let requestSequence = 0;
let pendingCreationFeedback = null;

// Saved rows render relative paths; show the absolute URL staff will share.
for (const link of links.querySelectorAll("a")) link.textContent = link.href;

// The toast's CSS animation rises from the bottom, holds, then fades; its end
// empties the live region.
toast.addEventListener("animationend", () => { toast.textContent = ""; });

function confirmCopy() {
  toast.textContent = "Link has been copied.";
  // Another copy while the toast shows replays it from the bottom.
  for (const animation of toast.getAnimations()) animation.currentTime = 0;
}

function showCreationFeedback() {
  if (!pendingCreationFeedback) return;
  const { link, success } = pendingCreationFeedback;
  if (!link.isConnected) {
    pendingCreationFeedback = null;
    return;
  }
  if (link.closest("li")?.parentElement !== links || link.getClientRects().length === 0) return;

  pendingCreationFeedback = null;
  if (success) confirmCopy();
  else status.textContent = "Link created. Clipboard unavailable; copy the link below.";
}

links.addEventListener("htmx:afterSwap", showCreationFeedback);

// A staff action invalidates earlier clipboard feedback as soon as it starts.
function startAction() {
  toast.textContent = "";
  pendingCreationFeedback = null;
  return ++requestSequence;
}

links.addEventListener("click", async ({ target }) => {
  const button = target.closest("[data-copy-link]");
  if (!button) return;

  const sequence = startAction();
  let copied = true;
  try {
    await navigator.clipboard.writeText(button.closest("li").querySelector("a").href);
  } catch {
    copied = false;
  }
  if (sequence === requestSequence) {
    if (copied) confirmCopy();
    else status.textContent = "Clipboard unavailable; select the link to copy it manually.";
  }
});

document.body.addEventListener("htmx:beforeRequest", ({ target }) => {
  // Live list refreshes aren't staff actions, so they keep pending feedback.
  if (target === links) return;
  const sequence = startAction();
  if (target !== createForm) return;

  status.textContent = "Creating link…";
  let receiveLink;
  const linkReady = new Promise((resolve) => { receiveLink = resolve; });
  let copied = Promise.resolve(false);

  if (navigator.clipboard?.write && window.ClipboardItem) {
    // Start during the click: Safari requires user activation before the response arrives.
    copied = navigator.clipboard.write([new ClipboardItem({
      "text/plain": linkReady.then((url) => {
        if (!url) throw new Error("Link creation failed.");
        return new Blob([url], { type: "text/plain" });
      }),
    })]).then(() => true, () => false);
  }

  createForm.addEventListener("htmx:afterRequest", async ({ detail }) => {
    if (!detail.successful) {
      receiveLink(null);
      return;
    }

    // The URL waits in #link-tokens until the list refresh moves it into its row.
    const link = tokens.querySelector("a:last-of-type");
    link.textContent = link.href;
    receiveLink(link.href);
    const success = await copied;
    if (sequence !== requestSequence) return;
    pendingCreationFeedback = { link, success };
    showCreationFeedback();
  }, { once: true });
});

document.body.addEventListener("htmx:afterRequest", ({ detail }) => {
  if (!detail.successful) {
    status.textContent = detail.xhr.responseText || "Request failed. Please try again.";
  }
});
