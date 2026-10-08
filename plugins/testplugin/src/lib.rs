// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

//! BareProxy's test plugin in Rust, written with the Proxy-Wasm SDK. It does
//! what the Go test plugin (src/internal/plugin/testdata/fixture) does, so the
//! plugin host's tests run against both. What it does is set by its config,
//! a list of words:
//!
//!   headers    add x-plugin to the request, and note the request's path
//!   deny       answer 403 with "denied by plugin" and an x-denied header
//!   resp       add x-resp to the response, and upper-case the response body
//!   call       call 127.0.0.1:PORT/called (PORT from the request's
//!              x-call-port), wait, then add x-called with the call's status
//!   store      put the path in the store, read it back into x-stored
//!   read       put the file hello.txt into x-read, with the call's status
//!   count      count requests in shared data, into x-count
//!   log        note "logged" in proxy_on_log
//!   tick       set a 10 ms timer that counts ticks in shared data
//!   loop       loop forever in proxy_on_request_headers
//!   crash      panic in proxy_on_request_headers
//!   grow       allocate 48 MB in proxy_on_request_headers
//!   refuse     refuse the config
//!   hold       pause the request and never go on
//!   crashresp  panic in proxy_on_response_headers
//!   denyresp   answer 451 from proxy_on_response_headers
//!   partial    add x-partial to the request, then panic
//!   setprop    set the property source.address to 6.6.6.6
//!   flaky      refuse to start any instance after the first

use proxy_wasm::hostcalls;
use proxy_wasm::traits::*;
use proxy_wasm::types::*;
use std::collections::HashSet;
use std::rc::Rc;
use std::time::Duration;

proxy_wasm::main! {{
    proxy_wasm::set_log_level(LogLevel::Info);
    proxy_wasm::set_root_context(|_| -> Box<dyn RootContext> { Box::new(Root { modes: Rc::new(HashSet::new()) }) });
}}

struct Root {
    modes: Rc<HashSet<String>>,
}

/// count adds one to a shared-data counter and returns the new value.
fn count(ctx: &dyn Context, key: &str) -> u64 {
    loop {
        let (val, cas) = ctx.get_shared_data(key);
        let n = val
            .and_then(|v| String::from_utf8(v).ok())
            .and_then(|s| s.parse::<u64>().ok())
            .unwrap_or(0)
            + 1;
        if ctx
            .set_shared_data(key, Some(n.to_string().as_bytes()), cas)
            .is_ok()
        {
            return n;
        }
    }
}

fn status_code(st: Status) -> u32 {
    st as u32
}

impl Context for Root {}

impl RootContext for Root {
    fn on_configure(&mut self, _: usize) -> bool {
        let config = self.get_plugin_configuration().unwrap_or_default();
        let modes: HashSet<String> = String::from_utf8_lossy(&config)
            .split_whitespace()
            .map(String::from)
            .collect();
        if modes.contains("tick") {
            self.set_tick_period(Duration::from_millis(10));
        }
        if modes.contains("refuse") {
            return false;
        }
        if modes.contains("flaky") && count(self, "starts") > 1 {
            panic!("the test plugin won't start again, as asked");
        }
        self.modes = Rc::new(modes);
        true
    }

    fn on_tick(&mut self) {
        count(self, "ticks");
    }

    fn create_http_context(&self, _: u32) -> Option<Box<dyn HttpContext>> {
        Some(Box::new(Http {
            modes: self.modes.clone(),
        }))
    }

    fn get_type(&self) -> Option<ContextType> {
        Some(ContextType::HttpContext)
    }
}

struct Http {
    modes: Rc<HashSet<String>>,
}

impl Http {
    fn has(&self, mode: &str) -> bool {
        self.modes.contains(mode)
    }

    fn property(&self, parts: Vec<&str>) -> String {
        self.get_property(parts)
            .map(|b| String::from_utf8_lossy(&b).into_owned())
            .unwrap_or_default()
    }

    fn note(&self, text: &str) {
        let _ = self.call_foreign_function("bareproxy_note", Some(text.as_bytes()));
    }
}

impl Context for Http {
    fn on_http_call_response(&mut self, _: u32, _: usize, body_size: usize, _: usize) {
        let status = self
            .get_http_call_response_header(":status")
            .unwrap_or_default();
        let body = self
            .get_http_call_response_body(0, body_size)
            .unwrap_or_default();
        let body = String::from_utf8_lossy(&body);
        self.add_http_request_header("x-called", &format!("{} {}", status, body.trim()));
        self.resume_http_request();
    }
}

impl HttpContext for Http {
    fn on_http_request_headers(&mut self, _: usize, _: bool) -> Action {
        if self.has("loop") {
            #[allow(clippy::empty_loop)]
            loop {
                std::hint::spin_loop();
            }
        }
        if self.has("crash") {
            panic!("the test plugin crashes, as asked");
        }
        if self.has("grow") {
            let v = vec![1u8; 48 << 20];
            std::hint::black_box(&v);
        }
        if self.has("partial") {
            self.add_http_request_header("x-partial", "1");
            panic!("the test plugin crashes halfway, as asked");
        }
        if self.has("setprop") {
            self.set_property(vec!["source", "address"], Some(b"6.6.6.6"));
        }
        if self.has("headers") {
            self.add_http_request_header("x-plugin", "hello");
            let method = self.get_http_request_header(":method").unwrap_or_default();
            let path = self.property(vec!["request", "path"]);
            let from = self.property(vec!["source", "address"]);
            self.note(&format!("saw {} {} from {}", method, path, from));
        }
        if self.has("count") {
            let n = count(self, "requests");
            self.add_http_request_header("x-count", &n.to_string());
        }
        if self.has("store") {
            let path = self.property(vec!["request", "path"]);
            let key = "last";
            let mut args = (key.len() as u32).to_le_bytes().to_vec();
            args.extend_from_slice(key.as_bytes());
            args.extend_from_slice(path.as_bytes());
            let _ = self.call_foreign_function("bareproxy_store_put", Some(&args));
            let v = self
                .call_foreign_function("bareproxy_store_get", Some(key.as_bytes()))
                .ok()
                .flatten()
                .unwrap_or_default();
            self.add_http_request_header("x-stored", &String::from_utf8_lossy(&v));
        }
        if self.has("read") {
            let (v, st) =
                match hostcalls::call_foreign_function("bareproxy_read_file", Some(b"hello.txt")) {
                    Ok(v) => (v.unwrap_or_default(), 0),
                    Err(st) => (Vec::new(), status_code(st)),
                };
            self.add_http_request_header(
                "x-read",
                &format!("{} {}", String::from_utf8_lossy(&v).trim(), st),
            );
        }
        if self.has("deny") {
            self.send_http_response(
                403,
                vec![("x-denied", "1"), ("content-type", "text/plain")],
                Some(b"denied by plugin\n"),
            );
            return Action::Pause;
        }
        if self.has("call") {
            let up = format!(
                "127.0.0.1:{}",
                self.get_http_request_header("x-call-port")
                    .unwrap_or_default()
            );
            let headers = vec![
                (":method", "GET"),
                (":path", "/called"),
                (":authority", "callee"),
            ];
            match self.dispatch_http_call(&up, headers, None, vec![], Duration::from_secs(2)) {
                Ok(_) => return Action::Pause,
                Err(st) => {
                    self.add_http_request_header(
                        "x-called",
                        &format!("refused {}", status_code(st)),
                    );
                    return Action::Continue;
                }
            }
        }
        if self.has("hold") {
            return Action::Pause;
        }
        Action::Continue
    }

    fn on_http_response_headers(&mut self, _: usize, _: bool) -> Action {
        if self.has("crashresp") {
            panic!("the test plugin crashes on the response, as asked");
        }
        if self.has("denyresp") {
            self.send_http_response(451, vec![], Some(b"replaced by plugin\n"));
            return Action::Pause;
        }
        if self.has("resp")
            && let Some(code) = self.get_property(vec!["response", "code"])
            && let Ok(b) = <[u8; 8]>::try_from(code.as_slice())
        {
            self.add_http_response_header("x-resp", &u64::from_le_bytes(b).to_string());
        }
        Action::Continue
    }

    fn on_http_response_body(&mut self, size: usize, end_of_stream: bool) -> Action {
        if !self.has("resp") {
            return Action::Continue;
        }
        if !end_of_stream {
            return Action::Pause; // the whole body, please
        }
        let body = self.get_http_response_body(0, size).unwrap_or_default();
        let upper = String::from_utf8_lossy(&body).to_uppercase();
        self.set_http_response_body(0, size, upper.as_bytes());
        Action::Continue
    }

    fn on_log(&mut self) {
        if self.has("log") {
            self.note("logged");
        }
    }
}
