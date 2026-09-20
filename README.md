# Jev Triage

A small command-line tool, written in Go, that reads a list of messages and sorts them by urgency. It uses **Jev**, a decision model from [TypeSafe AI](https://docs.typesafe.ai), to answer three questions about each message:

| Question | Type | Answer |
| --- | --- | --- |
| What kind of message is this? | Choice | `bug`, `billing`, `feature_request`, `account`, or `other` |
| How urgent is it? | Score | 0 (not urgent) to 3 (critical) |
| Does the sender sound frustrated? | Noul | A probability from 0 to 1 |

It prints a table in the terminal, puts low-confidence messages in a separate "review by hand" list, and writes an HTML report.

## What is Jev?

Most software is full of `if` statements: *if this is a refund request, send it to billing*. Those work well when the input is clean. They fall apart when the input is a real message like *"charged twice AGAIN. nobody answers!!"*

The usual fix is to ask a general LLM, then parse and validate its text reply. That means waiting for text to be generated, writing code to check it, retrying when it breaks, and only guessing how sure the model was.

Jev is built around the decision instead of the text. You send it a **state** (text or JSON) and some **named questions**. It sends back typed answers with probabilities:

- **Noul**: a yes/no question. Returns the probability of yes (0 to 1).
- **Choice**: pick one option from a list (up to 255). Returns the best option, a confidence, and a probability for every option.
- **Score**: rate the state on an ordered rubric you write (2 to 10 levels). Returns a number that can fall between levels, like 1.8.

Why that helps:

- **Nothing to parse.** Answers are values, not text.
- **Real uncertainty.** You get a probability, so you can automate the clear cases and send unsure ones to a person. This tool does exactly that with its `-threshold` flag.
- **Speed and cost.** TypeSafe reports Jev as up to 193.6x faster and 444.6x cheaper than LLMs on its own workflow evaluations. These are vendor numbers, so test them on your own data.
- **Many questions, one request.** All questions about a message run in parallel.

Jev decides. It does not write text, so it sits next to an LLM instead of replacing it.

## Requirements

- Go 1.21 or newer
- A TypeSafe API key (Jev is a paid, usage-priced API)

No third-party Go packages are used.

## Quick start

```bash
git clone https://github.com/boldbug1/jev-triage.git
cd jev-triage
```

Put your key in a file named `.env` in the project folder:

```
TYPESAFE_API_KEY=your-key-here
```

Then run it:

```bash
go run .
```

You can also set the key in your terminal instead of using `.env`:

| Shell | Command |
| --- | --- |
| PowerShell | `$env:TYPESAFE_API_KEY="your-key-here"` |
| Command Prompt | `set TYPESAFE_API_KEY=your-key-here` |
| macOS / Linux | `export TYPESAFE_API_KEY="your-key-here"` |

A variable set in the terminal takes priority over the `.env` file. Never commit `.env`; the included `.gitignore` already excludes it.

## Usage

```
go run . [flags]
```

| Flag | Default | What it does |
| --- | --- | --- |
| `-file` | `messages.txt` | Text file with one message per line |
| `-html` | `report.html` | Where to write the HTML report. Use `-html ""` to skip it |
| `-threshold` | `0.8` | Messages with category confidence below this go to the review list |
| `-workers` | `4` | How many requests to run at the same time |

Input file rules: one message per line. Blank lines and lines starting with `#` are ignored. A sample `messages.txt` is included.

## Output

The terminal shows all messages sorted by urgency, then the review list, then any failures. The HTML report has the same data with rows colored by urgency and a confidence bar for each message.

The output looks like this (sample data, your numbers will differ):

```
All messages, most urgent first (15)
URGENCY         CATEGORY  CONFIDENCE      FRUSTRATED  MESSAGE
3.0 critical    bug       #########. 90%  10%         Our production checkout has been down for 20 minutes and ev…
2.0 high        account   #########. 90%  10%         I can't log in since I changed my password this morning.
0.2 not urgent  billing   #########. 90%  95%         I was charged twice this month and nobody has answered my l…
```

## How it works

1. `main.go` reads the messages and starts `-workers` requests at a time.
2. For each message it sends one request to `POST https://api.typesafe.ai/v1/systemone` with the model `jev-latest` and all three questions.
3. Rate-limit, overload, and server errors (429, 529, 5xx) are retried up to 3 times with a short wait. Other errors, like a bad key, fail immediately and are listed at the end.
4. Results are sorted by urgency, split by confidence, and printed and written to HTML.

The questions live in `buildQuestions()` in `main.go`. To triage something else, change the category options, the urgency levels, or add a new question. Tip: keep an `other` option in every Choice question so unusual messages have somewhere to go.

## Project layout

```
main.go        the whole program
messages.txt   sample messages
go.mod         Go module file
.gitignore     keeps .env and build output out of Git
```

## Notes and limits

- Questions in one request are judged separately. If one question depends on another's answer, make a second request.
- The confidence threshold is a starting point. Look at your review list and adjust it for your data.
- `TYPESAFE_API_BASE` can be set to point the tool at a different base URL (a proxy or a local test server). It defaults to `https://api.typesafe.ai`.

## Disclaimer

This is an independent project and is not affiliated with or endorsed by TypeSafe AI. See the [TypeSafe documentation](https://docs.typesafe.ai) for the API itself.
