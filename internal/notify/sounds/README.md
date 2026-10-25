# Sounds

The built-in alert sounds are by [Universfield](https://pixabay.com/users/universfield-28281460/) on Pixabay.

| File | Used for | Original |
|---|---|---|
| `firing.ogg` | firing and critical alerts | [New Notification 056](https://pixabay.com/it/sound-effects/tecnologia-new-notification-056-494256/) |
| `resolved.ogg` | resolved alerts | [New Notification 042](https://pixabay.com/it/sound-effects/tecnologia-new-notification-042-493470/) |

## License

These files are under the [Pixabay Content License](https://pixabay.com/service/license-summary/), not the MIT license that covers the rest of this repository. They are bundled for use as healthd's alert sounds; do not redistribute them on their own. To use them elsewhere, get them from Pixabay under its license.

## Changes

The originals were downloaded from Pixabay as MP3 and converted to Ogg Vorbis (quality 4, metadata removed) so they play with any common Linux audio player:

```sh
ffmpeg -i firing.mp3 -map_metadata -1 -vn -c:a libvorbis -q:a 4 firing.ogg
```

To use different sounds without rebuilding, set `firing_sound`, `critical_sound` and `resolved_sound` under `[notify]` in the config.
