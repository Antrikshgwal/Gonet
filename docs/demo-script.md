# Demo script — explaining the HUD

A ~3-minute narration for a screen recording (or a live walkthrough) that
explains each HUD readout and ties it to the three ideas that make online games
feel instant: **client-side prediction**, **entity interpolation**, and
**lag compensation**. Bracketed lines are stage directions; the rest is spoken.

---

## HUD cheat sheet

| Readout | What it measures | The concept behind it |
|---|---|---|
| **RTT** | your round-trip ping (ms) | the latency prediction has to hide |
| **DRIFT** | gap (px) between your *predicted* dot and the *server's* dot | how far client-side prediction is running ahead |
| **CORR** | size (px) of the last reconciliation snap | server **reconciliation** correcting a misprediction |
| **lag** | artificial one-way latency you injected (`?lag=`) | a knob to make prediction visible |
| **jitter** | random variance added to that latency (`?jitter=`) | what **interpolation** has to absorb |
| **buf** | buffered position samples for the remote player | the **interpolation** cushion (depth of history) |
| 🟢 **predicted · you** | where the client *thinks* you are — drawn instantly | client-side prediction |
| 🟠 **server truth** | where the server *says* you are — the authority | reconciliation target |
| 🔵 **remote · interpolated** | the opponent, rendered ~150 ms in the past | entity interpolation |

---

## Scene 1 — the three dots (0:00)

> [Open `/play`, move around calmly.]
>
> "There are really three things on screen. The **green** dot is *you* — but it's
> not where the server says you are, it's where the client **predicts** you are,
> drawn the instant I press a key. The **amber** outline is the server's
> authoritative position. And in a second tab, the **blue** dot is my opponent."
>
> [Point at the telemetry panel.]
>
> "RTT is my ping — the round trip to the server. DRIFT is the distance between
> the green and amber dots — how far my prediction is running ahead of the
> server's truth. Right now, on localhost, ping is tiny so they sit right on top
> of each other: DRIFT is basically zero."

## Scene 2 — prediction hides lag (0:30)

> [Reload with `/play?lag=300`. Move.]
>
> "Now I've injected 300 milliseconds of lag. Watch RTT jump — and watch the
> **amber** ghost fall *behind* the green dot. That growing **DRIFT** number is
> the lag, made visible."
>
> "But here's the point: the **green dot — the one I control — is still perfectly
> instant.** That's **client-side prediction**. I don't wait for the round trip;
> I run the same physics locally and show the result immediately. Prediction is
> what cancels ping."
>
> [Stop moving.]
>
> "When I stop, the server catches up, and DRIFT collapses back to zero."

## Scene 3 — reconciliation keeps it honest (1:10)

> [Keep `?lag=300`. Point at CORR while moving.]
>
> "So if I'm just guessing locally, why doesn't my dot drift wrong forever? That's
> **CORR** — reconciliation. Every snapshot, the client snaps my dot to the
> server's authoritative position and **replays** the inputs the server hasn't
> confirmed yet. Because the physics is deterministic, my guess is usually right,
> so **CORR stays near zero** — the correction is invisible. No rubber-banding,
> even at half a second of lag."
>
> [Charge into the opponent / get hit.]
>
> "The one time CORR *spikes* is a collision or a respawn — a server event I
> couldn't have predicted. That's reconciliation earning its keep: it corrects
> the one thing I got wrong, instantly and smoothly."

## Scene 4 — interpolation smooths the opponent (1:50)

> [Switch focus to the opponent (blue) dot.]
>
> "I can predict *myself* because I know my own inputs. But I can't predict my
> **opponent** — I have no idea what keys they're pressing. All I get is their
> position, 20 times a second. If I drew that raw, they'd teleport in little
> jumps."
>
> "So instead the client **interpolates**: it buffers their recent positions —
> that's **buf**, the number of samples on hand — and renders them about
> **150 milliseconds in the past**, smoothly sliding between the two snapshots
> that bracket that moment. The trade is deliberate: I see my opponent slightly
> in the past, but they always move *smoothly*."

## Scene 5 — jitter, and why the buffer exists (2:25)

> [Reload with `/play?lag=300&jitter=150`.]
>
> "Real networks don't deliver packets on a clean schedule — the spacing wobbles.
> That's **jitter**. Watch **buf** fluctuate now as packets arrive unevenly."
>
> "But the opponent still moves smoothly — because that 150 ms interpolation
> window is a *cushion*. As long as packets land inside it, there's always
> history on both sides to slide between. **Prediction fights the delay;
> interpolation fights the jitter.**"

## Scene 6 — lag compensation (2:55, optional)

> [Under lag, charge into the opponent and land a hit.]
>
> "One last thing you can't see as a single number but you can *feel*: **lag
> compensation.** When I charge someone on my laggy screen, the server rewinds
> them to where I actually *saw* them before judging the hit — the same way an
> FPS confirms a shot server-side. So a hit that looked fair on my screen counts,
> even though, by the time my input arrived, they'd already moved."

## The one-liner to end on

> "Three problems, three tools: **prediction** hides *your* latency,
> **interpolation** hides *their* jitter, and **reconciliation** keeps your guess
> honest against the one source of truth — the server. That's the whole game of
> netcode."
