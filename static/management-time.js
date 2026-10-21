// The server renders UTC; show staff their own time zone, including swapped-in rows.
const timeFormat = new Intl.DateTimeFormat([], { dateStyle: "medium", timeStyle: "short" });
window.htmx?.onLoad((root) => {
  for (const time of root.querySelectorAll("time")) {
    time.textContent = timeFormat.format(new Date(time.dateTime));
  }
});
