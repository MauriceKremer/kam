# ECHO responder: when someone mentions the node, reply with hop count, SNR/RSSI and clock drift.
# Edit the three settings below, then install it as described in the README ("ECHO as a script").
CHANNELS = ["#public"]  # normalized names: lowercase, leading "#"
MENTION = "@kam"
NODE_NAME = "kam"  # messages from this sender are ignored

WORD_CHARS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"

def has_mention(body):
    index = body.find(MENTION)
    if index < 0:
        return False
    end = index + len(MENTION)
    return end >= len(body) or body[end] not in WORD_CHARS

def on_group_text(message):
    if message["channel"] not in CHANNELS or message["sender"] == NODE_NAME:
        return
    if not has_mention(message["body"]):
        return
    sender = message["sender"] or "someone"
    text = "heard %s in %d hops" % (sender, message["hop_count"])
    if message["has_rx_meta"]:
        snr = message["snr_quarter_db"]
        sign = ""
        if snr < 0:
            sign = "-"
            snr = 0 - snr
        fractions = ("00", "25", "50", "75")
        snr_text = "%s%d.%s" % (sign, snr // 4, fractions[snr % 4])
        text += ", SNR %s dB, RSSI %d dBm" % (snr_text, message["rssi_dbm"])
    text += ", drift %ds" % message["time_drift_seconds"]
    reply(message["channel"], text)
