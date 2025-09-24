#!/usr/bin/env python3
"""
Simple Jump Loss (Python CLI)

Usage:
  python3 jumplossPython.py <fee_units> [--seconds N | --blocks N]

  <fee_units>: thousandths-of-basis-points (e.g., 5000 -> 0.05%)
  --seconds N: dynamic half-life tied to N seconds (HL = N/2)
  --blocks N : dynamic half-life tied to N blocks (2s/block; HL = blocks)

Without flags, prints the default stream (HL=4800ms). With flags, prints dynamic JL once per second,
and supports live updates via stdin lines like "seconds 12" or "blocks 6".
"""
import sys
import time
import threading
from typing import Optional

from jumploss_python_lib import JumpLossWorker


def usage():
    print("Usage:\n  python3 jumplossPython.py <fee_units> [--seconds N | --blocks N]")


def main():
    if len(sys.argv) < 2:
        usage()
        sys.exit(1)
    try:
        fee_units = int(sys.argv[1])
    except ValueError:
        print("Invalid fee_units; must be integer")
        sys.exit(1)

    # parse optional dynamic mode
    dynamic = False
    time_type_sec: Optional[bool] = None
    st: Optional[int] = None
    if len(sys.argv) >= 4:
        flag = sys.argv[2]
        if flag in ("--seconds", "-s"):
            time_type_sec = True
            st = int(sys.argv[3])
            dynamic = True
        elif flag in ("--blocks", "-b"):
            time_type_sec = False
            st = int(sys.argv[3])
            dynamic = True
        else:
            print("Unknown flag:", flag)
            usage()
            sys.exit(1)

    jl = JumpLossWorker(fee_units=fee_units)
    jl.start_background()

    if dynamic:
        # live input thread to update settlement horizon
        def stdin_loop():
            nonlocal time_type_sec, st
            try:
                for line in sys.stdin:
                    parts = line.strip().split()
                    if len(parts) == 2:
                        key, val = parts[0].lower(), parts[1]
                        try:
                            n = int(val)
                        except ValueError:
                            continue
                        if key in ("seconds", "sec", "s"):
                            time_type_sec = True
                            st = n
                        elif key in ("blocks", "blk", "b"):
                            time_type_sec = False
                            st = n
            except Exception:
                pass

        threading.Thread(target=stdin_loop, daemon=True).start()

        try:
            while True:
                time.sleep(1)
                if st is None or time_type_sec is None:
                    continue
                upd = jl.request_jumploss(fee_units=fee_units, settlement_time=st, time_type_seconds=time_type_sec)
                hl_sec = (st // 2) if time_type_sec else st
                print(
                    f"JL(dynamic HL, HL={hl_sec}s): μ_J={upd.mu_j_percent:.6f}%  F={upd.f_percent:.6f}%  JL={upd.jl_percent:.6f}%  buckets={upd.bucket_count}"
                )
        except KeyboardInterrupt:
            pass
        finally:
            jl.stop()
        return

    # default stream: print updates from queue
    try:
        while True:
            upd = jl.updates.get()
            print(
                f"JL(simple,no-clamp): μ_J={upd.mu_j_percent:.6f}%  F={upd.f_percent:.6f}%  JL={upd.jl_percent:.6f}%  buckets={upd.bucket_count}"
            )
    except KeyboardInterrupt:
        pass
    finally:
        jl.stop()


if __name__ == "__main__":
    main()
