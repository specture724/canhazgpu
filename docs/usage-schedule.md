# Scheduled Bookings

Bookings let you claim GPUs for a **future time window**, the way you would book a meeting room. The `schedule` command prints the day's booking sheet, and `reserve --start` creates the bookings.

This complements the two existing ways to get GPUs:

| What you want | Command |
|---------------|---------|
| GPUs right now, tied to a process | `canhazgpu run` |
| GPUs right now, for a while | `canhazgpu reserve --duration 4h` |
| GPUs from 14:00 to 16:00 | `canhazgpu reserve --start 14:00 --end 16:00` |

## Viewing the schedule

```bash
canhazgpu schedule
```

```text
GPU Booking Schedule - 2026-08-03 (Mon)

      00 01 02 03 04 05 06 07 08 09 10 11 12 13 14 15 16 17 18 19 20 21 22 23    NOW
GPU0  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· aa !! bb ·· ·· ·· ·· ·· ·· ·· ··    alice MANUAL 8452MB
GPU1  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· bb bb ·· ·· ·· ·· ·· ·· ·· ··    free
GPU2  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ··    carol UNRESERVED 2048MB
GPU3  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ··    dave MANUAL booked idle 4m
                                             ^ now 13:06

  a  alice        13:06-15:06           GPU 0        manual reservation  note: interactive work
  b  bob          14:00-16:00           GPU 0,1      booking 4f89853e (pending)  note: fine-tune
  ·  free
```

How to read it:

- One row per GPU, one **pair of characters per hour**, so each character covers 30 minutes
- Each letter is one entry, explained in the legend below the grid; your own entries are green
- `·` is free time, `^ now` marks the current time
- `!` means two entries overlap on that GPU — usually a booking that is about to take over from a reservation that started earlier

The **NOW** column on the right shows what each GPU is doing at this moment, which the grid alone cannot tell you:

| NOW cell | Meaning |
|----------|---------|
| `free` | Nothing reserved, no usage |
| `free (45MB used)` | Not reserved, but some memory is in use below the threshold |
| `alice MANUAL 8452MB` | Reserved by alice and genuinely in use |
| `dave MANUAL booked idle 4m` | Reservation came from a booking, and no usage has been seen for 4 minutes |
| `carol UNRESERVED 2048MB` | Somebody is using the GPU without any reservation |

The column is only printed for the day that contains the current time, so `--date tomorrow` shows the grid alone. If the GPU provider cannot be queried, the column falls back to reservation records (user and type, without memory figures).

**Options:**

- `--date`: day to show (`2026-08-04`, `today`, `tomorrow`, `+2d`; default `today`)
- `--days`: number of days to print, starting at `--date` (default 1)
- `--json`: machine readable schedule
- `--cancel <id>`: cancel a booking
- `--force`: allow cancelling somebody else's booking
- `--no-color`: plain output

```bash
# Tomorrow's sheet
canhazgpu schedule --date tomorrow

# The rest of the week
canhazgpu schedule --days 5

# For scripts
canhazgpu schedule --json
```

## Making a booking

Use `reserve --start`. The window length comes from `--end`, or from `--duration` if no `--end` is given:

```bash
# 14:00 to 16:00 today, two GPUs
canhazgpu reserve --start 14:00 --end 16:00 --gpus 2

# Four hours starting tomorrow morning
canhazgpu reserve --start 'tomorrow 09:00' --duration 4h --gpus 8

# Specific GPUs, explicit date
canhazgpu reserve --start 2026-08-05T09:30 --duration 2h --gpu-ids 0,1

# Starting in two hours
canhazgpu reserve --start +2h --duration 1h --gpus 1
```

```text
Booked 2 GPU(s): [2 3]  14:00-16:00 (starts in 53m, 2h long)
Booking ID: 4f89853e
Released automatically if no GPU usage is detected for 15m after it starts

View the schedule:   canhazgpu schedule
Cancel the booking:  canhazgpu schedule --cancel 4f89853e
```

### Time formats

`--start` and `--end` accept:

| Format | Meaning |
|--------|---------|
| `14:00` | today at 14:00, or tomorrow if 14:00 already passed |
| `14:00:30` | same, with seconds |
| `tomorrow 09:30` | explicit day (`today 09:30` also works) |
| `2026-08-04 14:00` | explicit date and time (`T` also accepted as separator) |
| `2026-08-04` | midnight on that date |
| `+2h` | relative to now |
| `now` | immediately |

`--end` is interpreted relative to the start, so `--start 22:00 --end 02:00` books across midnight.

## What a booking guarantees

**GPUs are chosen when the booking is made** and recorded on the booking, so the schedule can show exactly which hardware is claimed. Bookings prefer GPUs that are free and least booked that day.

**Ad-hoc reservations cannot overrun a booking.** When you `reserve` or `run`, GPUs needed by a booking during your reservation's window are held back:

```bash
❯ canhazgpu reserve --gpu-ids 0 --duration 2h
Error: GPU 0 is held for a scheduled booking by bob (14:00-16:00, booking 4f89853e)
```

- For `reserve`, the window checked is exactly `now` to the end of the requested duration. A short reservation that ends before the booking starts is allowed.
- For `run`, there is no known end time, so GPUs booked within the next 30 minutes are avoided. Change this with `--booking-protection-window`.

**When the window starts, the booking takes over.** Any reservation still holding a booked GPU is released and the booking becomes a normal manual reservation that expires at the end of the window:

```text
Booking 4f89853e started: released manual reservation by alice on GPU 0
```

This is why the booking output warns you up front when somebody will be interrupted:

```text
Note: GPU 0 is reserved by alice until 15:06, which will be released when the booking starts
```

!!! warning "Processes are not killed by a booking"
    Preemption releases the *reservation*, not the process. Processes started with `canhazgpu run` are supervised and their reservation loss is reported, but a booking cannot stop a job that was launched without a reservation. Such a GPU shows up as `UNRESERVED` in `canhazgpu status` and the booking prints a warning when it activates.

!!! note "Activation is driven by canhazgpu commands"
    Bookings are activated whenever any canhazgpu command runs maintenance (`status`, `reserve`, `run`, `queue`, `schedule`, and the `web` dashboard's refresh loop). If nothing runs canhazgpu during the entire window, the booking simply expires unused. Running `canhazgpu web` (or a periodic `canhazgpu status`) keeps activation prompt.

## Cancelling

```bash
❯ canhazgpu schedule --cancel 4f89853e
Cancelled booking 4f89853e: bob, GPU 0,1, 14:00-16:00
```

- The ID is the abbreviated booking ID shown in the schedule and in the booking output
- Cancelling an **active** booking releases its GPUs immediately
- You can only cancel your own bookings unless you pass `--force`
- Releasing a booked GPU with `canhazgpu release` also ends the booking

## Idle bookings are released too

A booking carries the `--idle-timeout` it was created with. Once activated, if nobody uses the GPUs within that time, the reservation is dropped and the GPUs return to the pool, so a forgotten booking does not waste the whole window. See [Idle Reservation Timeout](features-idle-timeout.md).

## JSON output

```bash
canhazgpu schedule --json
```

```json
{
  "gpu_count": 4,
  "from": "2026-08-03T00:00:00+08:00",
  "to": "2026-08-04T00:00:00+08:00",
  "days": [
    {
      "date": "2026-08-03",
      "bookings": [
        {
          "id": "4f89853e-a5a3-46fd-b962-45ec529f3d0d",
          "short_id": "4f89853e",
          "user": "bob",
          "actual_user": "bob",
          "gpu_ids": [2, 3],
          "start": "2026-08-03T14:00:00+08:00",
          "end": "2026-08-03T16:00:00+08:00",
          "status": "pending",
          "note": "fine-tune",
          "idle_timeout_seconds": 900
        }
      ],
      "reservations": [
        {
          "gpu_id": 0,
          "user": "alice",
          "type": "manual",
          "start": "2026-08-03T13:06:05+08:00",
          "end": "2026-08-03T15:06:05+08:00"
        }
      ]
    }
  ]
}
```

Booking statuses are `pending` (window has not started), `active` (running now), `completed` (window over, or released early) and `cancelled`. Finished bookings are kept for 7 days and then pruned.
