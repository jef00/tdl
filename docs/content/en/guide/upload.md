---
title: "Upload"
weight: 40
---

# Upload

## Upload Files

Upload specified files and directories to `Saved Messages`:

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir
{{< /command >}}

## Custom Destination

Upload to custom chat.

{{< include "snippets/chat.md" >}}

### Specific Chat

Upload to specific one chat:

{{< command >}}
tdl up -p /path/to/file -c CHAT
{{< /command >}}

Upload to specific topic in a forum chat:

{{< command >}}
tdl up -p /path/to/file -c CHAT --topic TOPIC_ID
{{< /command >}}

### Message Routing

Upload to different chats by message router which is based on [expression](/reference/expr).

{{< hint warning >}}
The `--to` flag is conflicted with the `-c/--chat` and `--topic` flags. You can only use one of them.
{{< /hint >}}

List all available fields:

{{< command >}}
tdl up -p /path/to/file --to -
{{< /command >}}

Upload to `CHAT1` if MIME contains `video`, otherwise upload to `Saved Messages`:

{{< hint info >}}
You must return a **string** or **struct** as the target CHAT, and empty string means upload to `Saved Messages`.
{{< /hint >}}

{{< command >}}
tdl up -p /path/to/file \
--to 'MIME contains "video" ? "CHAT1" : ""'
{{< /command >}}

Upload to `CHAT1` if MIME contains `video`, otherwise upload to reply to message/topic `4` in `CHAT2`:

{{< command >}}
tdl up -p /path/to/file \
--to 'MIME contains "video" ? "CHAT1" : { Peer: "CHAT2", Thread: 4 }'
{{< /command >}}

Pass a file name if the expression is complex:

{{< details "router.txt" >}}
Write your expression like `switch`:

```javascript
MIME contains "video" ? "CHAT1" :
FileExt contains ".mp3" ? "CHAT2" :
FileName contains "chat3" > 30 ? {Peer: "CHAT3", Thread: 101} :
""
```

{{< /details >}}

{{< command >}}
tdl up -p /path/to/file --to router.txt
{{< /command >}}

## Custom Parameters

Upload with 8 threads per task, 4 concurrent tasks:

{{< command >}}
tdl up -p /path/to/file -t 8 -l 4
{{< /command >}}

## Custom Caption

Custom caption is based on [expression](/reference/expr).

List all available fields:

{{< command >}}
tdl up -p /path/to/file --caption -
{{< /command >}}

Custom simple caption:
{{< command >}}
tdl up -p /path/to/file --caption 'FileName + " - uploaded by tdl"'
{{< /command >}}

Write styled message with [HTML](https://core.telegram.org/bots/api#html-style):
{{< command >}}
tdl up -p /path/to/file --caption  \
'FileName + `<b>Bold</b> <a href="https://example.com">Link</a>`'
{{< /command >}}

Pass a file name if the expression is complex:

{{< details "caption.txt" >}}

```javascript
repeat(FileName, 2) + `
<a href="https://www.google.com">Google</a>
<a href="https://www.bing.com">Bing</a>
<b>bold</b>
<i>italic</i>
<code>code</code>
<tg-spoiler>spoiler</tg-spoiler>
<pre><code class="language-go">
package main

import "fmt"

func main() {
    fmt.Println("hello world")
}
</code></pre>
` + MIME
```

{{< /details >}}

{{< command >}}
tdl up -p /path/to/file --caption caption.txt
{{< /command >}}

## Filters

Upload files with extension filters:

{{< hint warning >}}
The extension is only matched with the file name, not the MIME type. So it may not work as expected.

Whitelist and blacklist can not be used at the same time.
{{< /hint >}}

Whitelist: Only upload files with `.jpg` `.png` extension

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir -i jpg,png
{{< /command >}}

Blacklist: Upload all files except `.mp4` `.flv` extension

{{< command >}}
tdl up -p /path/to/file -p /path/to/dir -e mp4 -e flv
{{< /command >}}

## Delete Local

Delete the uploaded file after uploading successfully:

{{< command >}}
tdl up -p /path/to/file --rm
{{< /command >}}

Also remove directories left empty under the given paths:

{{< command >}}
tdl up -p /path/to/dir --rm --prune
{{< /command >}}

## Split Large Files

Upload files over Telegram's size limit as store-only RAR volumes (`movie.part1.rar`, `movie.part2.rar`, ...). tdl builds the volumes itself while uploading: each one is a small RAR header followed by a slice of the source file, streamed straight to Telegram. Nothing is written to disk, no extra free space is needed and no `rar` / WinRAR installation is required. The result is a regular RAR 5 volume set that WinRAR, `unrar` and 7-Zip extract and verify.

The upload starts right away and the source is read only once: the checksums RAR stores are computed from the data as it is uploaded, and the start of each volume, which holds them, is sent last. Files are handled one at a time, right before their upload.

With `--rm`, the source is deleted only after every volume has been uploaded. An interrupted or failed run leaves nothing to clean up: run the same command again.

`--keep-volumes` additionally saves each uploaded volume next to its source file.

The default volume size is the account limit: 2000 MiB, or 4000 MiB with Premium. `--split-size` (MiB) makes volumes smaller and implies `--split`:

{{< command >}}
tdl up -p /path/to/dir --split
tdl up -p /path/to/dir --split-size 3900 --rm --prune
{{< /command >}}

To restore a file, download all volumes into one folder and extract the first one; the rest are picked up automatically.

Captions can use `Part` and `Parts`, e.g. `--caption 'FileName + (Parts > 0 ? " [" + string(Part) + "/" + string(Parts) + "]" : "")'`.

## Photo

Upload images as photos instead of documents:

{{< command >}}
tdl up -p /path/to/file --photo
{{< /command >}}
