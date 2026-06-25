"""Train a tiny behavior-cloning MLP from recorded play.

Each JSONL line is {"f": [6 features], "a": [dx, dy]}. The exported model is a
6 -> H -> 2 tanh network; the Go bot runs the identical forward pass.
"""
import json
import sys

import numpy as np


def load(path):
    X, Y = [], []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            d = json.loads(line)
            X.append(d["f"])
            Y.append(d["a"])
    return np.array(X, dtype=np.float64), np.array(Y, dtype=np.float64)


def balance(X, Y, rng):
    """Idle (0,0) dominates real play; downsample it to the active count so the
    model learns to move instead of collapsing to 'stand still'."""
    idle = np.all(Y == 0, axis=1)
    Xa, Ya = X[~idle], Y[~idle]
    Xi, Yi = X[idle], Y[idle]
    keep = min(len(Xi), len(Xa))
    sel = rng.permutation(len(Xi))[:keep]
    return np.vstack([Xa, Xi[sel]]), np.vstack([Ya, Yi[sel]])


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else "games.jsonl"
    out = sys.argv[2] if len(sys.argv) > 2 else "bot_model.json"
    X, Y = load(src)
    if len(X) == 0:
        sys.exit("no samples — record some play first")
    X = X[:, :4]  # positional features only; tolerates older 6-wide recordings

    H, lr, epochs, bs = 16, 0.05, 400, 256
    rng = np.random.default_rng(0)
    X, Y = balance(X, Y, rng)
    n = len(X)
    print(f"training on {n} balanced samples")
    W1, b1 = rng.normal(0, 0.5, (H, X.shape[1])), np.zeros(H)
    W2, b2 = rng.normal(0, 0.5, (2, H)), np.zeros(2)

    for ep in range(epochs):
        idx = rng.permutation(n)
        for s in range(0, n, bs):
            bi = idx[s:s + bs]
            xb, yb, m = X[bi], Y[bi], len(bi)
            h = np.tanh(xb @ W1.T + b1)          # (m, H)
            o = np.tanh(h @ W2.T + b2)           # (m, 2)
            do = (o - yb) * (1 - o ** 2) * (2 / m)
            gW2, gb2 = do.T @ h, do.sum(0)
            dh = (do @ W2) * (1 - h ** 2)
            gW1, gb1 = dh.T @ xb, dh.sum(0)
            W2 -= lr * gW2; b2 -= lr * gb2
            W1 -= lr * gW1; b1 -= lr * gb1
        if ep % 50 == 0 or ep == epochs - 1:
            o = np.tanh(np.tanh(X @ W1.T + b1) @ W2.T + b2)
            mse = ((o - Y) ** 2).mean()
            pred = np.where(np.abs(o) < 0.4, 0, np.sign(o))
            acc = (pred == Y).mean()
            print(f"epoch {ep:3d}  mse {mse:.4f}  axis-acc {acc:.3f}")

    model = {"h": H, "w1": W1.tolist(), "b1": b1.tolist(), "w2": W2.tolist(), "b2": b2.tolist()}
    with open(out, "w") as f:
        json.dump(model, f)
    print(f"wrote {out} from {n} samples")


if __name__ == "__main__":
    main()
