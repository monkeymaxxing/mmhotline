# Be away or take over on call

**Who acts:** any person on the team, in MMHotline's pages.

**When to use this page:** to see who is on call, to cover for someone for
a while, or to tell MMHotline that you can't respond for a while.

**What this page does:** you read who an SOS wakes now, and change it for a
chosen time without changing the schedule.

**Terms:**

- **Taking on call** makes you the first person an SOS wakes, until the end
  time you choose. It does not claim any incident.
- **Being unavailable** makes MMHotline skip you when it wakes people for
  an SOS, until the end time you choose.
- Times on MMHotline's pages are in the team's time zone, which the foot
  of each page names.

## Choose the route

- **See who is on call:** [See who an SOS wakes](#see-who-an-sos-wakes).
- **Cover for the person on call:** [Take on call](#take-on-call).
- **You can't respond for a while:** [Be unavailable](#be-unavailable).
- **A lasting change,** such as a new person in the rotation: an
  administrator changes the schedule, as
  [Set up people and the on-call schedule](set-up-people-and-the-on-call-schedule.md)
  says.

## See who an SOS wakes

1. **Open On call** in MMHotline.

   **Check:** **On call now** names the person on call, until when, and
   who follows, with a line that says why when it is not the plain
   schedule:

   ```text
   On call now
   Barbara Example
   Until 03:27, in 1 hour. Then Carlos Example.
   You took on call. The schedule names Carlos Example.
   ```

2. **Read Who an SOS wakes, in order:** the person on call; then, after the
   time to claim, the backup; then everyone else who is available. An
   unavailable person shows as skipped, with when that ends.

   ```text
   Who an SOS wakes, in order
   You
   no claim in 5 minutes → everyone else who is available:
   Carlos Example
   MMHotline keeps waking everyone who is available until someone claims the incident.
   ```

3. **Check The team** before relying on someone: each person's state, and
   what can wake them at night, such as
   `Woken by Pushover notification and automated voice call`. That line
   lists contact methods that are not failing; it does not prove the phone
   sounds.

From the server, `sudo mmhotline schedules show` ends with the same:

```text
On call now: Barbara Example (barbara@example.com). Backup now: everyone else available: Carlos Example (carlos@example.com). The person who took on-call is contacted first.
```

## Take on call

Barbara covers for Carlos, who is on call, for two hours.

1. **In Barbara's browser, open On call.** Under **Take on call**, enter `2h`
   in **For how long**, and choose **Take on call**.

   **Check:** **On call now** names Barbara, with the end time, and says
   `You took on call. The schedule names Carlos Example.`

2. **To end early,** Barbara chooses **Give on call back to the schedule**.
   An administrator can do the same.

   **Check:** **On call now** names the person the schedule names.

When the end time passes, on call returns to the schedule by itself.

- **The person on call** sees **Hand over early** in place of the form,
  which says to ask the next responder to choose **Take on call**.
- **Only one person at a time** can have taken on call. While Barbara
  has, Carlos's button reads **Take on call from Barbara Example**, so he
  replaces her knowingly.
- **If Barbara becomes unavailable,** her time on call pauses and the
  schedule applies; if she is available again before the end time, she is
  back on call.
- **Incidents someone already claimed stay theirs.** To hand one over, see
  [Take over a claimed incident](handle-an-incident.md#take-over-a-claimed-incident).

## Be unavailable

1. **Open On call,** or **Availability** in the menu. **Your availability**
   says what MMHotline does while you are unavailable, such as
   `While you are unavailable, MMHotline skips you and wakes everyone else who is available first.`

2. **Enter how long** in **For how long**, such as `2h`, and choose
   **Set yourself unavailable**.

   **Check:** the panel says `Unavailable until 03:27`, and **On call now**
   names whoever MMHotline wakes in your place.

3. **If you return early,** choose **I'm available again**. To change the
   end time, enter a new length and choose **Change how long**.

While you are unavailable:

- **Your contact methods** get no notifications, unless a card's
  **Send even when ...** box is on, as
  [Add contact methods](add-contact-methods.md#change-a-contact-method)
  says.
- **If nobody at all is available,** an SOS wakes every person anyway.
  Reason: an SOS must never have nobody to wake.
- **Incidents you claimed stay yours;** resolve them or hand them over.

## Lengths

Taking on call and being unavailable last from one minute to 30 days,
written as a number and a unit: `15m`, `2h`, `1h30m`, or `3d`. Neither can
start later; for a lasting change, change the schedule. Ending one leaves
the other, and a team mute, in place. **Until** times on the page round
down, so 2 hours 59 minutes from now shows `in 2 hours`.

## If something is different

- **The page says** `duration must be between one minute and 30 days`:
  choose a shorter or longer time.
- **The page says**
  `write the duration as a number and a unit, such as 30m, 2h, or 3d`:
  write it without spaces, such as `2h` rather than `2 hours`.
- **The page says**
  `The On call page changed while you had it open. Check the page, then choose again.`:
  someone changed on call, availability, or the team mute meanwhile;
  your change was not saved.
- **There is no Take on call form,** and it says
  `Make yourself available before taking on call.`: end your
  unavailability first.
