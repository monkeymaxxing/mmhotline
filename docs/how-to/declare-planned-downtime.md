# Declare planned downtime

**Who acts:** any person on the team, in MMHotline's pages; or the
monitoring tool itself, when it lists its own server's maintenance.

**When to use this page:** you are about to stop a monitoring tool that
checks in with MMHotline, such as to move or upgrade its server, and
nobody should be woken because its check-ins stop.

**What this page does:** you tell MMHotline until when one tool's silence
is expected, so it sends no alert about missing check-ins until then.

**Terms:**

- A **check-in** is the signed message a monitoring tool sends every 30
  seconds to say it still runs. When check-ins stop, MMHotline sends a
  Warning and then an SOS, as
  [Connect a tool that sends alerts](connect-a-tool-that-sends-alerts.md)
  sets up.
- **Planned downtime** stops those alerts about missing check-ins for one
  tool, until a chosen time. Alerts the tool still sends arrive as usual.

## Choose the route

- **You stop the tool, or its server, by hand:**
  [Start planned downtime](#start-planned-downtime) in MMHotline.
- **The tool's own server goes into maintenance, and the tool knows it:**
  [Let the tool report its maintenance](#let-the-tool-report-its-maintenance).
- **You want no phone to ring for a while, for any reason:** an
  administrator mutes the team, as [Mute the team](mute-the-team.md) says.

## Start planned downtime

1. **Open the tool's page.** In MMHotline, open **Settings → Alert
   sources**, then the tool, such as **Monitoring**. The page is
   **Check-ins and planned downtime**.

2. **Fill in Planned downtime.** Set **Until** to the date and time the
   tool should be back, in the team's time zone named beside the field,
   at most 7 days ahead. It starts one hour from now; for 30 minutes,
   change the time to 30 minutes from now. Optionally enter **Reason (optional)**, such as
   `moving the monitoring server`, at most 200 characters. Choose
   **Start planned downtime**.

   **Check:** the panel says:

   ```text
   Planned downtime
   Until 02:45, in 59 minutes, started by Carlos Example: moving the monitoring server.
   End planned downtime now
   ```

   The alert history records it as an Info alert, which wakes nobody:

   ```text
   Carlos Example started planned downtime for alert source “Monitoring” until 2026-11-15 07:45:00 UTC
   ```

   `sudo mmhotline alert-sources list` also shows the downtime:

   ```text
   Planned downtime until 2026-11-15T07:45:00Z, started by Carlos Example: moving the monitoring server.
   ```

3. **Do the work.** While the downtime lasts, the tool's silence sends
   nothing.

4. **When the tool is back early,** choose **End planned downtime now**.
   Otherwise the downtime ends by itself at its end time.

   **Check:** the history records
   `Carlos Example ended planned downtime for alert source “Monitoring”`,
   and the page's **Last check-in from ...** line shows the next check-ins.

Downtime always ends, so a forgotten downtime can't silence a tool for
good. While it lasts, the page offers only the end button; to change the
end time, end it and start a new one. A tool switched off in the settings
file can't get downtime; the page asks you to enable it first.

## After the downtime ends

If the tool is still silent, MMHotline sends an SOS, with no Warning first,
once both of these have passed: one minute after the downtime ended, and
the tool's usual SOS time since it was last heard from. The tool's alerts
count as hearing from it too, but only a check-in clears an alert about
missing check-ins. The Warning and SOS times are the tool's
`warn_after_no_check_in_for_minutes` and
`sos_after_no_check_in_for_minutes` in the settings file, as
[Connect a tool that sends alerts](connect-a-tool-that-sends-alerts.md)
says. Reason: you expected
the tool back by then, so its silence now is a real problem.

## Let the tool report its maintenance

A tool can list servers in maintenance in each check-in, in
`nodes_in_whole_node_maintenance`. When that list holds the server the
check-ins come from, `sent_by_node_name`, MMHotline counts it as planned
downtime until the window's `maintenance_until`:

```json
{"sent_at":"2026-11-15T06:50:00Z","sent_by_node_name":"monitor01","nodes_in_whole_node_maintenance":[{"node_name":"monitor01","maintenance_until":"2026-11-15T08:00:00Z","reason":"moving the monitoring server","started_by":"barbara"}]}
```

- **A window for other servers, or for only some checks,** does not
  count: those servers' own problems still reach MMHotline as alerts.
- **The page names the tool's window;** end it in the tool, not in
  MMHotline.
- **If the check-ins stop,** MMHotline keeps the last window it received
  until that window's end, but at most 7 days after the check-in that
  listed it. Reason: a window must always end. While check-ins keep
  listing the window, MMHotline moves that limit on.

## If something is different

- **The page says**
  `planned downtime must end in the future, at most 7 days from now`:
  choose an end time in that range. Reason: downtime must always end.
- **The page says**
  `the planned downtime reason must be at most 200 characters`: shorten
  the reason.
- **The tool's ordinary alerts still arrive during downtime:** planned
  downtime stops only alerts about missing check-ins. To stop phones
  ringing, an administrator mutes the team.
- **An SOS about missing check-ins came after the downtime:** the tool did
  not come back in time. It resolves by itself when check-ins return,
  unless someone claimed it; then the person who claimed it resolves it.
