package com.xxdrive.app

import android.os.Bundle
import android.widget.Button
import android.widget.EditText
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.json.JSONObject
import java.util.concurrent.TimeUnit

/** Login screen: server URL + credentials → stores bearer token, opens MainActivity. */
class LoginActivity : AppCompatActivity() {

    private val http = OkHttpClient.Builder()
        .connectTimeout(10, TimeUnit.SECONDS)
        .readTimeout(30, TimeUnit.SECONDS)
        .build()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Session.init(this)
        if (Session.isLoggedIn) {
            goMain()
            return
        }
        setContentView(R.layout.activity_login)

        val url = findViewById<EditText>(R.id.urlInput)
        val user = findViewById<EditText>(R.id.userInput)
        val pass = findViewById<EditText>(R.id.passInput)
        val btn = findViewById<Button>(R.id.loginBtn)
        val err = findViewById<TextView>(R.id.errorText)

        btn.setOnClickListener {
            val base = url.text.toString().trim().trimEnd('/')
            val u = user.text.toString().trim()
            val p = pass.text.toString()
            if (base.isEmpty() || u.isEmpty() || p.isEmpty()) {
                err.text = getString(R.string.err_all_fields)
                return@setOnClickListener
            }
            btn.isEnabled = false
            err.text = ""
            CoroutineScope(Dispatchers.Main).launch {
                try {
                    val token = withContext(Dispatchers.IO) { login(base, u, p) }
                    Session.baseUrl = base
                    Session.token = token
                    goMain()
                } catch (e: Exception) {
                    err.text = e.message ?: getString(R.string.err_generic)
                } finally {
                    btn.isEnabled = true
                }
            }
        }
    }

    @Throws(Exception::class)
    private fun login(base: String, username: String, password: String): String {
        val body = JSONObject().put("username", username).put("password", password)
            .toString().toRequestBody("application/json".toMediaType())
        val req = Request.Builder().url("$base/api/auth/login").post(body).build()
        http.newCall(req).execute().use { resp ->
            val text = resp.body?.string() ?: ""
            if (!resp.isSuccessful) throw Exception(getString(R.string.err_login_failed, resp.code))
            val json = JSONObject(text)
            return json.getString("token")
        }
    }

    private fun goMain() {
        startActivity(android.content.Intent(this, MainActivity::class.java))
        finish()
    }
}
