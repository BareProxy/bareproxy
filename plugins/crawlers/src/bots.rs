// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! The AI crawlers the plugin knows, as their operators describe them, and
//! the address lists the operators publish. Checked against each operator's
//! own page on 14 October 2026. A bot with no list here has none published
//! in a form the plugin can read; its claims can't be checked, only named.

/// Where a bot's addresses come from.
pub enum List {
    /// None published.
    None,
    /// A list the operator publishes, fetched and refreshed.
    Url(&'static str),
    /// Ranges the operator states on its page, kept here.
    Fixed(&'static [&'static str]),
}

/// A bot the plugin knows.
pub struct Known {
    /// Its name as its operator writes it. The same word, in any case, is
    /// what the plugin looks for in the User-Agent.
    pub name: &'static str,
    pub operator: &'static str,
    /// training, search or assistant.
    pub group: &'static str,
    pub list: List,
}

/// The groups, and what puts a bot in each.
pub const GROUPS: &[(&str, &str)] = &[
    (
        "training",
        "collects pages to train AI models or to build data sets for them",
    ),
    (
        "search",
        "indexes pages for an AI search or answer engine, which links to them",
    ),
    (
        "assistant",
        "fetches a page when a person asks an AI assistant or agent to",
    ),
];

const ANTHROPIC: &str = "https://claude.com/crawling/bots.json";

pub const KNOWN: &[Known] = &[
    // https://developers.openai.com/api/docs/bots
    Known {
        name: "GPTBot",
        operator: "OpenAI",
        group: "training",
        list: List::Url("https://openai.com/gptbot.json"),
    },
    Known {
        name: "OAI-SearchBot",
        operator: "OpenAI",
        group: "search",
        list: List::Url("https://openai.com/searchbot.json"),
    },
    Known {
        name: "ChatGPT-User",
        operator: "OpenAI",
        group: "assistant",
        list: List::Url("https://openai.com/chatgpt-user.json"),
    },
    // https://support.claude.com/en/articles/8896518: one list for all three.
    Known {
        name: "ClaudeBot",
        operator: "Anthropic",
        group: "training",
        list: List::Url(ANTHROPIC),
    },
    Known {
        name: "Claude-SearchBot",
        operator: "Anthropic",
        group: "search",
        list: List::Url(ANTHROPIC),
    },
    Known {
        name: "Claude-User",
        operator: "Anthropic",
        group: "assistant",
        list: List::Url(ANTHROPIC),
    },
    // https://docs.perplexity.ai/docs/resources/perplexity-crawlers (the
    // lists on www.perplexity.com redirect here).
    Known {
        name: "PerplexityBot",
        operator: "Perplexity",
        group: "search",
        list: List::Url("https://www.perplexity.ai/perplexitybot.json"),
    },
    Known {
        name: "Perplexity-User",
        operator: "Perplexity",
        group: "assistant",
        list: List::Url("https://www.perplexity.ai/perplexity-user.json"),
    },
    // https://developers.google.com/search/docs/crawling-indexing/google-user-triggered-fetchers.
    // Google-Extended, Google's switch for Gemini training, is a robots.txt
    // word with no user agent of its own, so it isn't here.
    Known {
        name: "Google-Agent",
        operator: "Google",
        group: "assistant",
        list: List::Url(
            "https://developers.google.com/static/crawling/ipranges/user-triggered-agents.json",
        ),
    },
    Known {
        name: "Google-GeminiNotebook",
        operator: "Google",
        group: "assistant",
        list: List::None,
    },
    // https://commoncrawl.org/ccbot
    Known {
        name: "CCBot",
        operator: "Common Crawl",
        group: "training",
        list: List::Url("https://index.commoncrawl.org/ccbot.json"),
    },
    // https://docs.mistral.ai/robots/
    Known {
        name: "MistralAI-Training",
        operator: "Mistral AI",
        group: "training",
        list: List::None,
    },
    Known {
        name: "MistralAI-Index",
        operator: "Mistral AI",
        group: "search",
        list: List::Url("https://mistral.ai/mistralai-index-ips.json"),
    },
    Known {
        name: "MistralAI-User",
        operator: "Mistral AI",
        group: "assistant",
        list: List::Url("https://mistral.ai/mistralai-user-ips.json"),
    },
    // https://developer.amazon.com/support/amazonbot: each list is a page
    // with the JSON in it.
    Known {
        name: "Amazonbot",
        operator: "Amazon",
        group: "training",
        list: List::Url("https://developer.amazon.com/amazonbot/ip-addresses/"),
    },
    Known {
        name: "Amzn-SearchBot",
        operator: "Amazon",
        group: "search",
        list: List::Url("https://developer.amazon.com/amazonbot/searchbot-ip-addresses/"),
    },
    Known {
        name: "Amzn-User",
        operator: "Amazon",
        group: "assistant",
        list: List::Url("https://developer.amazon.com/amazonbot/live-ip-addresses/"),
    },
    // https://developers.facebook.com/docs/sharing/webmasters/web-crawlers:
    // Meta names its network (AS32934) but publishes no list to fetch.
    Known {
        name: "Meta-ExternalAgent",
        operator: "Meta",
        group: "training",
        list: List::None,
    },
    Known {
        name: "Meta-WebIndexer",
        operator: "Meta",
        group: "search",
        list: List::None,
    },
    Known {
        name: "Meta-ExternalFetcher",
        operator: "Meta",
        group: "assistant",
        list: List::None,
    },
    // ByteDance documents nothing; Bytespider says who it is and crawls
    // for its models.
    Known {
        name: "Bytespider",
        operator: "ByteDance",
        group: "training",
        list: List::None,
    },
    // https://duckduckgo.com/duckduckgo-help-pages/results/duckassistbot:
    // the addresses are in the page's text, not a list to fetch.
    Known {
        name: "DuckAssistBot",
        operator: "DuckDuckGo",
        group: "assistant",
        list: List::None,
    },
    // https://you.com/docs/youbot: one range, stated on the page.
    Known {
        name: "YouBot",
        operator: "You.com",
        group: "search",
        list: List::Fixed(&["68.67.112.0/24"]),
    },
    // https://allenai.org/crawler
    Known {
        name: "AI2Bot",
        operator: "Ai2",
        group: "training",
        list: List::None,
    },
];
