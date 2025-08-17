# SpleenToTextBot Usage Instructions

This Discord bot transcribes audio files sent as attachments in a channel. You can also enhance the transcript using GenAI.

## How to Use

1. **Trigger the Bot**

   - Send a message in a channel the bot can read, starting with:
     - `!transcribe` — for basic transcription
     - `!transcribe genai` — to enhance the transcript with GenAI

2. **Attach an Audio File**

   - Supported formats: `.ogg`, `.mp3`, `.wav`
   - The audio file must be attached to the same message as the command.

3. **Receive the Transcript**
   - The bot will reply in the channel with the transcript (or enhanced transcript if `genai` is used).

## Example Commands

- Basic transcription:

```
!transcribe
```

_(attach your audio file)_

- Enhanced transcription with GenAI:

```text
!transcribe genai
```

_(attach your audio file)_

## Notes

- The bot ignores messages from itself.
- Only processes messages that start with `!transcribe` and have a supported audio attachment.
- Make sure your Google API key is set in the environment for transcription and enhancement to work.
