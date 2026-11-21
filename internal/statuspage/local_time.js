// Shows the status page's times in the visitor's time zone. index.html holds
// every time in UTC, as the text of <time datetime="..."> elements, and is
// complete without this script. The script runs inline, so it is a plain
// script, not a module. It reads attributes and writes textContent only. Any
// failure leaves the UTC page untouched: the new texts are all worked out
// before the first one is written.
try {
  const options = { hour: "numeric", minute: "2-digit" };
  const formats = {
    full: new Intl.DateTimeFormat(undefined, { day: "numeric", month: "long", year: "numeric", ...options }),
    time: new Intl.DateTimeFormat(undefined, options),
    dayAndTime: new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short", ...options }),
  };
  const zone = new Intl.DateTimeFormat(undefined, { timeZoneName: "short" })
    .formatToParts(new Date())
    .find((part) => part.type === "timeZoneName").value;
  const day = (date) => date.getFullYear() + "-" + date.getMonth() + "-" + date.getDate();

  const times = Array.from(document.querySelectorAll("time[datetime]"));
  const texts = times.map((element) => {
    const date = new Date(element.getAttribute("datetime"));
    if (element.getAttribute("data-format") === "update") {
      // An update shows only the time of day on the day its notice opened, in
      // the visitor's time zone; on another day it names the day.
      const opened = new Date(element.getAttribute("data-day-ref"));
      return (day(date) === day(opened) ? formats.time : formats.dayAndTime).format(date);
    }
    return formats.full.format(date);
  });

  times.forEach((element, index) => { element.textContent = texts[index]; });
  document.querySelectorAll(".tz").forEach((element) => { element.hidden = true; });
  const note = document.getElementById("time-zone-note");
  if (note) note.textContent = "Times are in your local time zone (" + zone + ").";
} catch (_) {}
