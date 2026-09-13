<?php

if (!defined('WHMCS')) {
    die('This file cannot be accessed directly');
}

function opanelent_MetaData()
{
    return [
        'DisplayName' => 'OPanel Enterprise Hosting',
        'APIVersion' => '1.1',
        'RequiresServer' => true,
    ];
}

function opanelent_ConfigOptions()
{
    return [
        'Package' => [
            'Type' => 'text',
            'Size' => '25',
            'Loader' => 'opanelent_PackageLoader',
            'SimpleMode' => true,
            'Default' => '1',
            'Description' => 'Select an OPanel Enterprise package',
        ],
        'App Type' => [
            'Type' => 'dropdown',
            'Options' => 'php,static,wordpress',
            'Default' => 'php',
        ],
        'PHP Version' => [
            'Type' => 'dropdown',
            'Options' => '8.4,8.3,8.2,8.1,8.0,7.4,5.6',
            'Default' => '8.4',
        ],
        'Install WordPress' => [
            'Type' => 'yesno',
            'Description' => 'Install WordPress during provisioning',
        ],
        'Auto SSL' => [
            'Type' => 'yesno',
            'Description' => 'Request Let\'s Encrypt SSL after provisioning',
        ],
    ];
}

function opanelent_PackageLoader($params)
{
    $result = opanelent_request($params, 'GET', '/api/provisioning/v1/plans');
    if (!$result['ok']) {
        throw new Exception($result['error']);
    }

    return opanel_ent_package_options($result['data']);
}

function opanelent_TestConnection($params)
{
    $result = opanelent_request($params, 'GET', '/api/provisioning/v1/plans');
    return $result['ok'] ? ['success' => true] : ['success' => false, 'error' => $result['error']];
}

function opanelent_CreateAccount($params)
{
    $domain = opanel_ent_domain($params);
    $username = opanel_ent_provision_username($params);
    $password = opanelent_password($params);
    $payload = [
        'external_id' => opanelent_external_id($params),
        'username' => $username,
        'password' => $password,
        'package_id' => (int) opanel_ent_config($params, 1, '1'),
        'php_version' => opanel_ent_config($params, 3, '8.4'),
        'app_type' => $domain === '' ? 'php' : opanel_ent_config($params, 2, 'php'),
        'install_wordpress' => opanel_ent_yesno(opanel_ent_config($params, 4, '')),
        'enable_ssl' => opanel_ent_yesno(opanel_ent_config($params, 5, '')),
    ];

    if ($domain !== '') {
        $payload['domain'] = $domain;
    }

    if ($payload['install_wordpress']) {
        $payload['app_type'] = 'wordpress';
    }
    if ($domain === '' && ($payload['install_wordpress'] || $payload['enable_ssl'])) {
        return 'A domain is required when Install WordPress or Auto SSL is enabled';
    }

    $result = opanelent_request($params, 'POST', '/api/provisioning/v1/accounts', $payload);
    if (!$result['ok']) {
        return $result['error'];
    }

    $accountUsername = trim((string) ($result['data']['username'] ?? $username));
    opanel_ent_save_service_record($params, $result['data'], $accountUsername, $password);
    return 'success';
}

function opanelent_SuspendAccount($params)
{
    $payload = ['reason' => $params['suspendreason'] ?? 'Suspended by WHMCS'];
    $result = opanelent_request($params, 'POST', '/api/provisioning/v1/accounts/' . rawurlencode(opanelent_external_id($params)) . '/suspend', $payload);
    return $result['ok'] ? 'success' : $result['error'];
}

function opanelent_UnsuspendAccount($params)
{
    $result = opanelent_request($params, 'POST', '/api/provisioning/v1/accounts/' . rawurlencode(opanelent_external_id($params)) . '/unsuspend');
    return $result['ok'] ? 'success' : $result['error'];
}

function opanelent_TerminateAccount($params)
{
    // Terminate is a full teardown: databases dropped, site files and the home
    // directory removed. It used to send backup=true, which read as "keep the
    // data" -- the API has never accepted that parameter and silently deleted
    // everything anyway. Sending nothing says what actually happens.
    $result = opanelent_request($params, 'DELETE', '/api/provisioning/v1/accounts/' . rawurlencode(opanelent_external_id($params)));
    return $result['ok'] ? 'success' : $result['error'];
}

function opanelent_ChangePassword($params)
{
    $payload = ['password' => opanelent_password($params)];
    $result = opanelent_request($params, 'PATCH', '/api/provisioning/v1/accounts/' . rawurlencode(opanelent_external_id($params)) . '/password', $payload);
    return $result['ok'] ? 'success' : $result['error'];
}

function opanelent_ChangePackage($params)
{
    $payload = ['package_id' => (int) opanel_ent_config($params, 1, '1')];
    $result = opanelent_request($params, 'PATCH', '/api/provisioning/v1/accounts/' . rawurlencode(opanelent_external_id($params)) . '/package', $payload);
    return $result['ok'] ? 'success' : $result['error'];
}

function opanelent_UsageUpdate($params)
{
    if (empty($params['serviceid'])) {
        return 'success';
    }

    $result = opanelent_request($params, 'GET', '/api/provisioning/v1/accounts/' . rawurlencode(opanelent_external_id($params)) . '/usage');
    if (!$result['ok']) {
        return $result['error'];
    }

    opanel_ent_save_service_note($params, $result['data']);
    return 'success';
}

function opanelent_LoginLink($params)
{
    $url = opanel_ent_sso_url($params);
    return '<a href="' . htmlspecialchars($url, ENT_QUOTES, 'UTF-8') . '" target="_blank">Login to OPanel Enterprise</a>';
}

function opanelent_ClientArea($params)
{
    $account = [];
    if (!empty($params['serviceid'])) {
        $result = opanelent_request($params, 'GET', '/api/provisioning/v1/accounts/' . rawurlencode(opanelent_external_id($params)));
        if ($result['ok']) {
            $account = $result['data'];
        }
    }

    return [
        'templatefile' => 'clientarea',
        'vars' => [
            'panelUrl' => opanel_ent_base_url($params),
            'loginUrl' => opanel_ent_sso_url($params),
            'username' => opanel_ent_username($params),
            'serviceLabel' => opanel_ent_service_label($params, $account),
            'packageName' => trim((string) ($account['package_name'] ?? '')),
            'domain' => trim((string) ($account['domain'] ?? '')),
            'status' => trim((string) ($account['status'] ?? '')),
        ],
    ];
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

function opanelent_request($params, $method, $path, $payload = null, $query = [])
{
    if (!function_exists('curl_init')) {
        return ['ok' => false, 'error' => 'PHP cURL extension is required'];
    }

    $token = trim((string) ($params['serveraccesshash'] ?? ''));
    if ($token === '') {
        $token = trim((string) ($params['serverpassword'] ?? ''));
    }
    if ($token === '') {
        return ['ok' => false, 'error' => 'Missing OPanel Enterprise API token in server Access Hash or Password'];
    }

    $url = opanel_ent_base_url($params) . $path;
    if ($query) {
        $url .= '?' . http_build_query($query);
    }

    $headers = [
        'Authorization: Bearer ' . $token,
        'Accept: application/json',
    ];

    $ch = curl_init($url);
    curl_setopt_array($ch, [
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_CUSTOMREQUEST => strtoupper($method),
        CURLOPT_CONNECTTIMEOUT => 15,
        CURLOPT_TIMEOUT => 90,
        CURLOPT_HTTPHEADER => $headers,
        CURLOPT_SSL_VERIFYPEER => true,
        CURLOPT_SSL_VERIFYHOST => 2,
    ]);

    if ($payload !== null) {
        $body = json_encode($payload);
        curl_setopt($ch, CURLOPT_POSTFIELDS, $body);
        $headers[] = 'Content-Type: application/json';
        curl_setopt($ch, CURLOPT_HTTPHEADER, $headers);
    }

    $body = curl_exec($ch);
    $errno = curl_errno($ch);
    $error = curl_error($ch);
    $status = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
    curl_close($ch);

    if ($errno) {
        opanel_ent_log($params, $method, $path, $payload, null, 'cURL ' . $errno . ': ' . $error);
        return ['ok' => false, 'error' => 'OPanel Enterprise connection failed: ' . $error];
    }

    $data = json_decode((string) $body, true);
    if ($status < 200 || $status >= 300) {
        // Unwrap provisioning envelope error if present
        if (is_array($data) && isset($data['error']) && $data['error']) {
            $message = $data['error'];
        } elseif (is_array($data) && isset($data['detail'])) {
            $message = is_array($data['detail'])
                ? ($data['detail']['error'] ?? json_encode($data['detail']))
                : $data['detail'];
        } else {
            $message = (string) $body;
        }
        opanel_ent_log($params, $method, $path, $payload, $body, 'HTTP ' . $status . ': ' . $message);
        return ['ok' => false, 'error' => 'OPanel Enterprise API error: ' . $message];
    }

    // Unwrap provisioning envelope success
    if (is_array($data) && array_key_exists('data', $data)) {
        $data = $data['data'];
    }

    opanel_ent_log($params, $method, $path, $payload, $body, 'OK');
    return ['ok' => true, 'data' => is_array($data) ? $data : []];
}

function opanel_ent_base_url($params)
{
    $hostname = trim((string) ($params['serverhostname'] ?? ''));
    if ($hostname === '') {
        $hostname = trim((string) ($params['serverip'] ?? ''));
    }
    $hostname = preg_replace('#^https?://#', '', $hostname);
    $hostname = rtrim($hostname, '/');

    $secure = !empty($params['serversecure']);
    $scheme = $secure ? 'https' : 'http';
    $port = trim((string) ($params['serverport'] ?? ''));

    if ($port !== '' && !in_array($port, ['80', '443'], true)) {
        return $scheme . '://' . $hostname . ':' . $port;
    }

    return $scheme . '://' . $hostname;
}

function opanelent_external_id($params)
{
    return 'whmcs:' . (int) ($params['serviceid'] ?? 0);
}

function opanel_ent_username($params)
{
    $username = trim((string) ($params['username'] ?? ''));
    if ($username !== '' && preg_match('/^[a-z_][a-z0-9_-]{2,31}$/', $username)) {
        return $username;
    }

    $serviceId = (int) ($params['serviceid'] ?? 0);
    return opanel_ent_random_username($serviceId);
}

function opanel_ent_provision_username($params)
{
    $serviceId = (int) ($params['serviceid'] ?? 0);
    return opanel_ent_random_username($serviceId);
}

function opanelent_password($params)
{
    $password = (string) ($params['password'] ?? '');
    if (strlen($password) >= 12 && strlen($password) <= 72) {
        return $password;
    }

    return opanel_ent_random_string(24, 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789');
}

function opanel_ent_domain($params)
{
    return strtolower(trim((string) ($params['domain'] ?? '')));
}

function opanel_ent_config($params, $index, $default)
{
    $key = 'configoption' . $index;
    $value = $params[$key] ?? $default;
    return trim((string) $value) !== '' ? trim((string) $value) : $default;
}

function opanel_ent_yesno($value)
{
    return in_array(strtolower(trim($value)), ['on', 'yes', 'true', '1'], true);
}

function opanel_ent_package_options($packages)
{
    if (!is_array($packages)) {
        throw new Exception('OPanel Enterprise package list response is invalid');
    }

    $options = [];
    foreach ($packages as $package) {
        if (!is_array($package)) {
            continue;
        }

        $id = (int) ($package['id'] ?? 0);
        if ($id <= 0) {
            continue;
        }

        $name = trim((string) ($package['name'] ?? ''));
        if ($name === '') {
            $name = 'Package ' . $id;
        }

        $options[(string) $id] = $name . ' (#' . $id . ')';
    }

    if ($options === []) {
        throw new Exception('No OPanel Enterprise packages found');
    }

    return $options;
}

function opanel_ent_service_label($params, $account)
{
    if (is_array($account) && !empty($account['service_label'])) {
        return (string) $account['service_label'];
    }

    $product = trim((string) ($params['productname'] ?? ''));
    if ($product === '') {
        $product = 'OPanel Enterprise Hosting';
    }

    $serviceId = (int) ($params['serviceid'] ?? 0);
    return $serviceId > 0 ? $product . ' #' . $serviceId : $product;
}

function opanel_ent_sso_url($params)
{
    if (empty($params['serviceid'])) {
        return opanel_ent_base_url($params);
    }

    $result = opanelent_request($params, 'POST', '/api/provisioning/v1/accounts/' . rawurlencode(opanelent_external_id($params)) . '/login');
    if ($result['ok'] && !empty($result['data']['login_url'])) {
        $url = trim((string) $result['data']['login_url']);
        if (preg_match('#^https?://#i', $url)) {
            return $url;
        }
        return rtrim(opanel_ent_base_url($params), '/') . '/' . ltrim($url, '/');
    }

    return opanel_ent_base_url($params);
}

function opanel_ent_save_service_record($params, $data, $username = '', $password = '')
{
    if (!function_exists('localAPI') || empty($params['serviceid'])) {
        return;
    }

    $lines = ['OPanel Enterprise'];
    foreach (['service_label', 'external_id', 'username', 'email', 'domain', 'status', 'package_id', 'package_name', 'storage_used_bytes', 'storage_limit_bytes', 'storage_percent'] as $key) {
        if (array_key_exists($key, $data)) {
            $lines[] = $key . ': ' . (is_scalar($data[$key]) ? $data[$key] : json_encode($data[$key]));
        }
    }

    $update = [
        'serviceid' => (int) $params['serviceid'],
        'notes' => implode("\n", $lines),
    ];
    if ($username !== '') {
        $update['serviceusername'] = $username;
    }
    if ($password !== '') {
        $update['servicepassword'] = $password;
    }

    try {
        localAPI('UpdateClientProduct', $update);
    } catch (Throwable $e) {
        // WHMCS record update is best effort; provisioning already succeeded.
    }
}

function opanel_ent_save_service_note($params, $data)
{
    opanel_ent_save_service_record($params, $data);
}

function opanel_ent_random_username($serviceId)
{
    $prefix = 'op' . max(0, (int) $serviceId) . '_';
    return substr($prefix . opanel_ent_random_string(8, 'abcdefghijklmnopqrstuvwxyz0123456789'), 0, 32);
}

function opanel_ent_random_string($length, $alphabet)
{
    $result = '';
    $max = strlen($alphabet) - 1;
    for ($i = 0; $i < $length; $i++) {
        $result .= $alphabet[random_int(0, $max)];
    }
    return $result;
}

function opanel_ent_log($params, $method, $path, $request, $response, $result)
{
    if (!function_exists('logModuleCall')) {
        return;
    }

    $replaceVars = [trim((string) ($params['serveraccesshash'] ?? '')), trim((string) ($params['serverpassword'] ?? ''))];
    if (is_array($request) && isset($request['password'])) {
        $replaceVars[] = (string) $request['password'];
    }

    logModuleCall(
        'opanel-ent',
        $method . ' ' . $path,
        $request,
        $response,
        $result,
        $replaceVars
    );
}
