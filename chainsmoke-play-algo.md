5 different ways to call a song by url (choose two, fixed choice)


  Route	                        argv leak	  shell history	   works today
chainsmoke play <url>	        ❌ leaks	    ❌ leaks	        refused
echo <url> | chainsmoke play	✅ clean	    ❌ still leaks	✅
chainsmoke play (prompt)	    ✅ clean	    ✅ clean	        ✅ new
chainsmoke play < url.txt	    ✅ clean	    ✅ clean	        ✅
search → play N	                ✅ clean	    ✅ clean	        ⛔ needs T4.1


The prompt is the new best default for a human at a terminal. The pipe stays for scripts.

The friction you hit is really T4.1. The whole point of the numbered-results design is that you never handle a URL at all — chainsmoke search kitten then chainsmoke play 3. That's broken right now only because the default store is in-memory and dies with the process. Land the runtime store and this question stops existing.

If you'd rather have the convenience anyway, the honest version is an explicit opt-in — --unsafe-url — matching how the spec gates direct egress with --allow-direct. It'd still leak; it'd just be your informed choice rather than a silent one. Your call, I didn't add it.
