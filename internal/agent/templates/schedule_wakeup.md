Start or continue a self-paced background loop where you choose the delay before each check, rather than a fixed interval -- like "keep an eye on this and check back when it makes sense to." Use this for things like "watch until the deploy finishes" or "keep checking whether the PR was approved," where a shorter wait makes sense while something is actively in progress and a longer one otherwise.

Two ways to call it:
- **Start a new loop**: omit `task_id`. Give a `prompt` and how long to wait before the first check (`delay_seconds`). This returns a task ID.
- **Keep an existing loop going**: when a scheduled wakeup fires, its prompt is delivered as a normal message tagged with its task ID. Call ScheduleWakeup again with that `task_id`, a (possibly updated) `prompt`, and a new `delay_seconds` before you finish responding to it, if the loop should continue.

If a firing's turn ends without calling ScheduleWakeup again, the loop gets one more reminder firing and then stops on its own -- so if you're done, either call ScheduleCancel or just don't reschedule.

Each firing runs in the background and reports back into this session, queued behind any active turn so it never interrupts the user. For a fixed, predictable cadence instead (no per-firing decision needed), use ScheduleCron.
