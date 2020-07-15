-- Copyright 2017 Jeff Foley. All rights reserved.
-- Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

name = "Brute Forcing"
type = "brute"

probes = {"www", "online", "webserver", "ns", "ns1", "mail", "smtp", "webmail", "prod", "test", "vpn", "ftp", "ssh"}

function vertical(ctx, domain)
    local cfg = config(ctx)
    if cfg.mode == "passive" then
        return
    end

    if cfg['brute_forcing'].active then
        makenames(ctx, domain)
    end
end

function resolved(ctx, name, domain, records)
    local nparts = split(name, ".")
    local dparts = split(domain, ".")
    -- Do not process resolved root domain names
    if #nparts == #dparts then
        return
    end

    -- Do not generate names from CNAMEs or names without A/AAAA records
    if (#records == 0 or (has_cname(records) or not has_addr(records))) then
        return
    end

    local cfg = config(ctx)
    if cfg.mode == "passive" then
        return
    end

    local bf = cfg['brute_forcing']
    if (bf.active and bf.recursive and (bf['min_for_recursive'] == 0)) then
        makenames(ctx, name)
    end
end

function subdomain(ctx, name, domain, times)
    local cfg = config(ctx)
    if cfg.mode == "passive" then
        return
    end

    local bf = cfg['brute_forcing']
    if (bf.active and bf.recursive and (bf['min_for_recursive'] == times)) then
        makenames(ctx, name)
    end
end

function makenames(ctx, base)
    local wordlist = brute_wordlist(ctx)

    for i, word in pairs(wordlist) do
        sendnames(ctx, word .. "." .. base)
    end
end

function sendnames(ctx, content)
    local names = find(content, subdomainre)
    if names == nil then
        return
    end

    for i, v in pairs(names) do
        newname(ctx, v)
    end
end

function has_cname(records)
    if #records == 0 then
        return false
    end

    for i, rec in pairs(records) do
        if rec.rrtype == 5 then
            return true
        end
    end

    return false
end

function has_addr(records)
    if #records == 0 then
        return false
    end

    for i, rec in pairs(records) do
        if (rec.rrtype == 1 or rec.rrtype == 28) then
            return true
        end
    end

    return false
end

function split(str, delim)
    local result = {}
    local pattern = "[^%" .. delim .. "]+"

    local matches = find(str, pattern)
    if (matches == nil or #matches == 0) then
        return result
    end

    for i, match in pairs(matches) do
        table.insert(result, match)
    end

    return result
end
