const createForm = document.querySelector("#create-link");
const links = document.querySelector("#generated-links");
const status = document.querySelector("#link-status");
let requestSequence = 0;

// Saved rows render relative paths; show the absolute URL staff will share.
for (const link of links.querySelectorAll("a")) link.textContent = link.href;

links.addEventListener("click", async ({ target }) => {
  const button = target.closest("[data-copy-link]");
  if (!button) return;

  const sequence = ++requestSequence;
  let copied = true;
  try {
    await navigator.clipboard.writeText(button.closest("li").querySelector("a").href);
  } catch {
    copied = false;
  }
  if (sequence === requestSequence) {
    status.textContent = copied
      ? "Link copied to clipboard."
      : "Clipboard unavailable; select the link to copy it manually.";
  }
});

document.body.addEventListener("htmx:beforeRequest", ({ target }) => {
  const sequence = ++requestSequence;
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

    const link = links.lastElementChild.querySelector("a");
    link.textContent = link.href;
    receiveLink(link.href);
    const success = await copied;
    if (sequence === requestSequence && links.lastElementChild?.querySelector("a") === link) {
      status.textContent = success
        ? "Link copied to clipboard."
        : "Link created. Clipboard unavailable; copy the link below.";
    }
  }, { once: true });
});

document.body.addEventListener("htmx:afterRequest", ({ detail }) => {
  if (!detail.successful) {
    status.textContent = detail.xhr.responseText || "Request failed. Please try again.";
  }
});

// The server renders UTC; show staff their own time zone, including swapped-in rows.
const timeFormat = new Intl.DateTimeFormat([], { dateStyle: "medium", timeStyle: "short" });
window.htmx?.onLoad((root) => {
  for (const time of root.querySelectorAll("time")) {
    time.textContent = timeFormat.format(new Date(time.dateTime));
  }
});
